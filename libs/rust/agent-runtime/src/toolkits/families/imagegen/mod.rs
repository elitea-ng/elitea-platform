//! The `imagegen` toolkit family (#864): `generate_image` and `edit_image`
//! through the gateway's OpenAI-compatible images API, saved to the
//! toolkit's artifact bucket.
//!
//! Names, arguments, descriptions, the `{name_prefix}{generate|edit}-{i}.png`
//! file names and the `{"artifacts": [{"filepath", "filename"}]}` result keep
//! the SDK's (`elitea_sdk/tools/imagegen/api_wrapper.py`). The settings are
//! the SDK's four fields: `image_generation_model`, `bucket`, `name_prefix`
//! and `selected_tools`.
//!
//! There is no credential and no model of the family's own. The images API
//! and the object API are reached through the Office host
//! ([`super::office_host::OfficeToolHost`]), which the cloud worker lends over
//! the turn's own credential: the images call is accounted, budgeted and
//! logged by the gateway like every other model call of the turn, and the
//! image is written through the same platform object API the Office families
//! use. No new internal route.
//!
//! Differences from the SDK, stated: a name that is taken is never
//! overwritten (the image takes the next free index); `bucket` and
//! `name_prefix` are validated with the object API's own rules when the
//! toolkit is materialized; a selection of only unknown tools is refused;
//! an edit's inputs are size-checked together before they are read; an answer item that is only a `url` is
//! not fetched (the SDK downloads it; this runtime opens no connection beyond
//! the platform, and every request asks for `b64_json`, which is what the
//! gateway answers); `n` is capped at 10, the images API's own maximum;
//! failures are sentences the model reads, not raised exceptions.

use std::fmt;
use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_connectors::artifact::client::{valid_object_key, valid_segment};
use serde_json::{Map, Value, json};

use crate::toolkits::families::artifact::ArtifactToolAuthority;
use crate::toolkits::families::office_host::{
    ImageEditInput, ImageEditRequest, ImageGenerateRequest, MAX_IMAGE_EDIT_INPUT_BYTES,
    MAX_IMAGES_PER_REQUEST, MAX_OFFICE_OBJECT_BYTES, OfficeFailure, OfficeHostError,
    SharedOfficeToolHost, TOOLKIT_BUDGET_SENTENCE, valid_upload_file_name,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

const DEFAULT_NAME_PREFIX: &str = "generated-";
const MAX_DESCRIPTION_CHARS: usize = 1_000;
const MAX_SELECTED_TOOLS: usize = 1_024;
const MAX_TOOL_NAME_BYTES: usize = 64;
const MAX_SETTING_BYTES: usize = 256;
const MAX_PROMPT_BYTES: usize = 32 * 1_024;
const MAX_KEY_BYTES: usize = 1_024;
/// The most colliding file names one saved image steps over before it gives up.
const MAX_NAME_PROBES: usize = 1_000;
/// The most unknown tool names a refusal lists.
const MAX_LISTED_UNKNOWN: usize = 8;

const GENERATE_IMAGE: &str = "generate_image";
const EDIT_IMAGE: &str = "edit_image";
const GENERATE_DESCRIPTION: &str = "Generate one or more images from a text prompt and save them to the project's artifact bucket.";
const EDIT_DESCRIPTION: &str = "Edit an existing image (an artifact filepath) from a text prompt and save the result to the project's artifact bucket.";

/// One of the family's tools.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ImageGenTool {
    GenerateImage,
    EditImage,
}

const ALL_TOOLS: [ImageGenTool; 2] = [ImageGenTool::GenerateImage, ImageGenTool::EditImage];

impl ImageGenTool {
    fn parse(name: &str) -> Option<Self> {
        match name {
            GENERATE_IMAGE => Some(Self::GenerateImage),
            EDIT_IMAGE => Some(Self::EditImage),
            _ => None,
        }
    }

    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::GenerateImage => GENERATE_IMAGE,
            Self::EditImage => EDIT_IMAGE,
        }
    }

    const fn description(self) -> &'static str {
        match self {
            Self::GenerateImage => GENERATE_DESCRIPTION,
            Self::EditImage => EDIT_DESCRIPTION,
        }
    }

    fn parameters_schema(self) -> Value {
        match self {
            Self::GenerateImage => json!({
                "type": "object",
                "properties": {
                    "prompt": {"type": "string", "maxLength": MAX_PROMPT_BYTES, "description": "Text prompt describing the image to generate."},
                    "size": {"type": "string", "maxLength": 32, "description": "Image size, e.g. '1024x1024'. Leave empty for the model's default."},
                    "n": {"type": "integer", "default": 1, "description": "Number of images to generate."}
                },
                "required": ["prompt"],
                "additionalProperties": false
            }),
            Self::EditImage => json!({
                "type": "object",
                "properties": {
                    "image": {"type": "string", "maxLength": MAX_KEY_BYTES, "description": "Artifact filepath (/{bucket}/{filename}) of the source image to edit."},
                    "prompt": {"type": "string", "maxLength": MAX_PROMPT_BYTES, "description": "Text prompt describing the edit to apply."},
                    "mask": {"type": "string", "maxLength": MAX_KEY_BYTES, "description": "Optional artifact filepath (/{bucket}/{filename}) of a mask image; transparent areas of the mask mark where the edit applies."}
                },
                "required": ["image", "prompt"],
                "additionalProperties": false
            }),
        }
    }
}

/// Why the settings were refused.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ImageGenConfigErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
}

/// Stable failure for claim-materialized imagegen settings. It carries no
/// setting value; the one thing it can name is the unknown tool names of a
/// selection that leaves nothing to serve (bounded tool names, not secrets).
pub(crate) struct ImageGenConfigError {
    code: ImageGenConfigErrorCode,
    unknown_tools: Vec<String>,
}

impl ImageGenConfigError {
    pub(crate) const fn code(&self) -> ImageGenConfigErrorCode {
        self.code
    }
}

impl fmt::Debug for ImageGenConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ImageGenConfigError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ImageGenConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.code {
            ImageGenConfigErrorCode::InvalidConfiguration if !self.unknown_tools.is_empty() => {
                write!(
                    f,
                    "the imagegen toolkit selects only tools it does not serve ({}); it serves generate_image and edit_image",
                    self.unknown_tools.join(", ")
                )
            }
            ImageGenConfigErrorCode::InvalidConfiguration => {
                f.write_str("the imagegen toolkit configuration is invalid")
            }
            ImageGenConfigErrorCode::ResourceExhausted => {
                f.write_str("the imagegen toolkit configuration exceeds its approved limit")
            }
        }
    }
}

impl std::error::Error for ImageGenConfigError {}

const fn refusal(code: ImageGenConfigErrorCode) -> ImageGenConfigError {
    ImageGenConfigError {
        code,
        unknown_tools: Vec::new(),
    }
}

const INVALID: ImageGenConfigError = refusal(ImageGenConfigErrorCode::InvalidConfiguration);
const EXHAUSTED: ImageGenConfigError = refusal(ImageGenConfigErrorCode::ResourceExhausted);

/// The toolkit's settings. A model or bucket that is absent builds the tools
/// anyway and is reported by the tool, in the SDK's words, when it is called.
pub(crate) struct ImageGenToolkitConfig {
    model: Option<Box<str>>,
    bucket: Option<Box<str>>,
    name_prefix: Box<str>,
    /// `None`: every tool (an absent, null or empty selection).
    selected: Option<Vec<ImageGenTool>>,
}

impl ImageGenToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ImageGenConfigError> {
        let bucket = text_setting(settings, "bucket")?;
        // The bucket the platform's object API will accept, checked here with
        // the SAME rule its write applies, so a bad one is refused before any
        // billed call, not after the image is generated.
        if bucket
            .as_deref()
            .is_some_and(|bucket| !valid_segment(bucket))
        {
            return Err(INVALID);
        }
        let name_prefix = name_prefix_setting(settings)?;
        // Every file name the family writes is `{prefix}{generate|edit}-{n}.png`;
        // it must be an object key the platform accepts and a name the upload
        // (and every later `edit_image` of it) can carry.
        let probe = format!("{name_prefix}generate-0.png");
        if !valid_object_key(&probe) || !valid_upload_file_name(file_name(&probe)) {
            return Err(INVALID);
        }
        Ok(Self {
            model: text_setting(settings, "image_generation_model")?,
            bucket,
            name_prefix,
            selected: selected_tools(settings)?,
        })
    }

    fn tools(&self) -> Vec<ImageGenTool> {
        self.selected.clone().unwrap_or_else(|| ALL_TOOLS.to_vec())
    }
}

/// The SDK's `settings.get("name_prefix") or "generated-"`: an absent, null or
/// EMPTY prefix is the default; anything else is used exactly as given, with
/// no trimming (`"img "` and `" "` are prefixes).
fn name_prefix_setting(settings: &Map<String, Value>) -> Result<Box<str>, ImageGenConfigError> {
    match settings.get("name_prefix") {
        None | Some(Value::Null) => Ok(DEFAULT_NAME_PREFIX.into()),
        Some(Value::String(text)) if text.is_empty() => Ok(DEFAULT_NAME_PREFIX.into()),
        Some(Value::String(text)) if text.len() > MAX_SETTING_BYTES => Err(EXHAUSTED),
        Some(Value::String(text)) => Ok(text.as_str().into()),
        Some(_) => Err(INVALID),
    }
}

fn text_setting(
    settings: &Map<String, Value>,
    key: &str,
) -> Result<Option<Box<str>>, ImageGenConfigError> {
    match settings.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(text)) => {
            let text = text.trim();
            if text.len() > MAX_SETTING_BYTES {
                return Err(EXHAUSTED);
            }
            if text.chars().any(char::is_control) {
                return Err(INVALID);
            }
            Ok((!text.is_empty()).then(|| text.into()))
        }
        Some(_) => Err(INVALID),
    }
}

fn selected_tools(
    settings: &Map<String, Value>,
) -> Result<Option<Vec<ImageGenTool>>, ImageGenConfigError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(None);
    };
    if value.is_null() {
        return Ok(None);
    }
    let values = value.as_array().ok_or(INVALID)?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(EXHAUSTED);
    }
    if values.is_empty() {
        return Ok(None);
    }
    let mut selected: Vec<ImageGenTool> = Vec::new();
    let mut unknown: Vec<String> = Vec::new();
    for value in values {
        let name = value.as_str().ok_or(INVALID)?.trim();
        if name.is_empty() || name.chars().any(char::is_control) {
            return Err(INVALID);
        }
        if name.len() > MAX_TOOL_NAME_BYTES {
            return Err(EXHAUSTED);
        }
        match ImageGenTool::parse(name) {
            Some(tool) => {
                if !selected.contains(&tool) {
                    selected.push(tool);
                }
            }
            None => {
                if unknown.len() < MAX_LISTED_UNKNOWN && !unknown.iter().any(|n| n == name) {
                    unknown.push(name.to_owned());
                }
            }
        }
    }
    // A selection that names tools and none the family serves would bind
    // nothing and look like a working toolkit: refuse it, naming them.
    if selected.is_empty() {
        return Err(ImageGenConfigError {
            code: ImageGenConfigErrorCode::InvalidConfiguration,
            unknown_tools: unknown,
        });
    }
    Ok(Some(selected))
}

/// The ADK tool definitions were refused by admission.
pub(crate) struct ImageGenToolsetError;

impl fmt::Debug for ImageGenToolsetError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("ImageGenToolsetError")
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ImageGenToolsetError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("the imagegen ADK tool definition is invalid")
    }
}

impl std::error::Error for ImageGenToolsetError {}

impl From<MaterializedToolsetError> for ImageGenToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self
    }
}

/// Build the claim-scoped imagegen toolset. `None` when the authority lends no
/// host that reaches the images API: the toolkit is then left out as
/// unsupported, never bound to tools that would fail on every call.
pub(crate) fn build_imagegen_toolset(
    toolkit_name: &str,
    config: &ImageGenToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    authority: &ArtifactToolAuthority,
) -> Result<Option<BasicToolset>, ImageGenToolsetError> {
    let Some(host) = authority.office().filter(|host| host.serves_images()) else {
        return Ok(None);
    };
    build_with_host(toolkit_name, config, policy, host).map(Some)
}

pub(crate) fn build_with_host(
    toolkit_name: &str,
    config: &ImageGenToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    host: &SharedOfficeToolHost,
) -> Result<BasicToolset, ImageGenToolsetError> {
    let settings = Arc::new(Settings {
        model: config.model.clone(),
        bucket: config.bucket.clone(),
        name_prefix: config.name_prefix.clone(),
    });
    let tools: Vec<Arc<dyn Tool>> = config
        .tools()
        .into_iter()
        .map(|tool| -> Arc<dyn Tool> {
            Arc::new(ImageGenToolImpl {
                tool,
                settings: Arc::clone(&settings),
                host: Arc::clone(host),
                description: tool_description(toolkit_name, tool.description()),
            })
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "imagegen", policy, tools).map_err(Into::into)
}

/// The SDK's description rule: `Toolkit: <name>\n` before the description,
/// cut at 1000 characters.
fn tool_description(toolkit_name: &str, text: &str) -> Box<str> {
    format!("Toolkit: {toolkit_name}\n{text}")
        .chars()
        .take(MAX_DESCRIPTION_CHARS)
        .collect::<String>()
        .into_boxed_str()
}

struct Settings {
    model: Option<Box<str>>,
    bucket: Option<Box<str>>,
    name_prefix: Box<str>,
}

struct ImageGenToolImpl {
    tool: ImageGenTool,
    settings: Arc<Settings>,
    host: SharedOfficeToolHost,
    description: Box<str>,
}

/// A host failure as the model reads it: one sentence per real cause.
fn host_sentence(action: &str, error: &OfficeHostError) -> String {
    tracing::warn!(
        event = "imagegen_tool_host_failed",
        reason_code = error.reason_code(),
        action,
        "an imagegen tool's platform call failed"
    );
    match error.kind() {
        OfficeFailure::NotFound => {
            format!("Could not {action}: no such file in the bucket. Check the file path.")
        }
        OfficeFailure::AccessRefused => {
            format!("Could not {action}: the platform refused access. Do not retry; tell the user.")
        }
        OfficeFailure::AlreadyExists => {
            format!("Could not {action}: a file with that name already exists.")
        }
        OfficeFailure::UnsupportedType => {
            format!("Could not {action}: the file store does not accept that type of file.")
        }
        OfficeFailure::InvalidRequest => format!(
            "Could not {action}: the request was refused as invalid (check the image model, the size, the prompt and the file names)."
        ),
        OfficeFailure::TooLarge => {
            format!("Could not {action}: a file is larger than the platform accepts.")
        }
        OfficeFailure::BudgetRefused if error.is_toolkit_budget() => {
            format!("Could not {action}: {TOOLKIT_BUDGET_SENTENCE}.")
        }
        OfficeFailure::BudgetRefused => format!(
            "Could not {action}: the model budget is exhausted. Do not retry; tell the user."
        ),
        OfficeFailure::NoModel | OfficeFailure::ModelUnusable | OfficeFailure::ContextUnusable => {
            format!("Could not {action}: image generation is not available in this run.")
        }
        OfficeFailure::RateLimited => format!(
            "Could not {action}: the image model is rate-limited right now. Try again shortly."
        ),
        OfficeFailure::Timeout => {
            format!("Could not {action}: the image model timed out. It may work if tried again.")
        }
        OfficeFailure::Conflict => format!(
            "Could not {action}: the model gateway reported a conflict. It may work if tried again."
        ),
        OfficeFailure::Unsupported => {
            format!("Could not {action}: this runtime does not offer that operation.")
        }
        OfficeFailure::ReplyUnusable
        | OfficeFailure::ReplyCutOff
        | OfficeFailure::ReplyDeclined
        | OfficeFailure::ReplyEmpty => format!(
            "Could not {action}: the image model answered but its reply was unusable (it may have been billed). Do not retry without telling the user."
        ),
        OfficeFailure::ReplyTooLarge => format!(
            "Could not {action}: the image model's answer was larger than the platform accepts (it may have been billed). Do not retry without telling the user."
        ),
        OfficeFailure::Unreachable => format!(
            "Could not {action}: the platform or the image model could not be reached. Tell the user it did not happen."
        ),
    }
}

/// `/{bucket}/{key}` split into its bucket and key, as the SDK's
/// `get_raw_content_by_filepath` reads a path. A path with no leading slash is
/// a key in the toolkit's own bucket.
fn split_filepath<'a>(
    default_bucket: &'a str,
    path: &'a str,
) -> Result<(&'a str, &'a str), String> {
    let trimmed = path.trim();
    let (bucket, key) = match trimmed.strip_prefix('/') {
        Some(rooted) => rooted.split_once('/').ok_or_else(|| {
            format!("`{trimmed}` is not an artifact filepath (/{{bucket}}/{{filename}}).")
        })?,
        None => (default_bucket, trimmed),
    };
    if bucket.is_empty() || key.is_empty() || key.len() > MAX_KEY_BYTES {
        return Err(format!(
            "`{trimmed}` is not an artifact filepath (/{{bucket}}/{{filename}})."
        ));
    }
    if key.split('/').any(|segment| segment == "..") {
        return Err("The file path may not contain `..`.".to_owned());
    }
    Ok((bucket, key))
}

fn file_name(key: &str) -> &str {
    key.rsplit('/').next().unwrap_or(key)
}

/// A file name the image model's upload can carry (the one shared guard).
fn upload_name_ok(name: &str) -> Result<(), String> {
    if !valid_upload_file_name(name) {
        return Err(format!(
            "The file name `{name}` cannot be sent to the image model: it is empty or contains a quote, a backslash or a control character. Rename the file and try again."
        ));
    }
    Ok(())
}

fn string_arg(arguments: &Value, key: &str) -> Option<String> {
    arguments
        .get(key)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .filter(|value| !value.trim().is_empty())
}

impl ImageGenToolImpl {
    fn model(&self) -> Result<&str, String> {
        self.settings
            .model
            .as_deref()
            .ok_or_else(|| "Image generation model is not configured for this toolkit".to_owned())
    }

    fn bucket(&self) -> Result<&str, String> {
        self.settings
            .bucket
            .as_deref()
            .ok_or_else(|| "An artifact bucket is not configured for this toolkit".to_owned())
    }

    /// Write every inline image and answer the SDK's `{"artifacts": [...]}`.
    ///
    /// The first candidate name is `{prefix}{op}-{i}.png` with `i` the image's
    /// position in the answer's `data` array (the SDK enumerates it). Unlike
    /// the SDK, which overwrote, a name that is taken is never replaced: the
    /// image takes the next free index, so an earlier call's images are kept.
    /// A write that fails after others succeeded answers what was saved AND
    /// the failure, so the model never generates again what was billed.
    async fn save(&self, images: Vec<Option<Vec<u8>>>, operation: &str) -> Result<Value, String> {
        let bucket = self.bucket()?;
        let total = images.iter().flatten().count();
        if total == 0 {
            return Err("The image generation model returned no image data".to_owned());
        }
        let mut saved = Vec::new();
        let mut remaining = total;
        let mut next_free = 0_usize;
        for (position, bytes) in images.into_iter().enumerate() {
            let Some(bytes) = bytes else { continue };
            remaining -= 1;
            let mut index = position.max(next_free);
            let mut probes = 0;
            let outcome = loop {
                let name = format!("{}{operation}-{index}.png", self.settings.name_prefix);
                match self
                    .host
                    .write_object(bucket, &name, "image/png", bytes.clone(), false)
                    .await
                {
                    Ok(written) => break Ok(written),
                    Err(error)
                        if error.kind() == OfficeFailure::AlreadyExists
                            && probes < MAX_NAME_PROBES =>
                    {
                        probes += 1;
                        index += 1;
                    }
                    Err(error) => break Err((name, error)),
                }
            };
            match outcome {
                Ok(written) => {
                    next_free = index + 1;
                    saved.push(json!({
                        "filepath": format!("/{}/{}", written.bucket, written.name),
                        "filename": file_name(&written.name),
                    }));
                }
                Err((name, error)) => {
                    let sentence = host_sentence(&format!("save the image `{name}`"), &error);
                    if saved.is_empty() {
                        return Err(sentence);
                    }
                    let tail = match remaining {
                        0 => String::new(),
                        1 => " The 1 image after it was not saved.".to_owned(),
                        n => format!(" The {n} images after it were not saved."),
                    };
                    return Ok(json!({
                        "artifacts": saved,
                        "error": format!("saved {} of {total}. {sentence}{tail}", saved.len()),
                    }));
                }
            }
        }
        Ok(json!({"artifacts": saved}))
    }

    async fn generate(&self, arguments: &Value) -> Result<Value, String> {
        let model = self.model()?.to_owned();
        let prompt = string_arg(arguments, "prompt")
            .ok_or_else(|| "A non-empty `prompt` is required.".to_owned())?;
        // The SDK: `n or 1`, and no size for "auto".
        let count = match arguments.get("n") {
            None | Some(Value::Null) => 1,
            Some(value) => value
                .as_i64()
                .ok_or_else(|| "`n` must be a whole number.".to_owned())?,
        };
        let count = if count == 0 { 1 } else { count };
        let most = i64::try_from(MAX_IMAGES_PER_REQUEST).unwrap_or(10);
        if !(1..=most).contains(&count) {
            return Err(format!("`n` must be between 1 and {most}."));
        }
        let size = string_arg(arguments, "size")
            .map(|size| size.trim().to_owned())
            .filter(|size| !size.eq_ignore_ascii_case("auto"));
        // Fail on a missing bucket before the (billed) model call.
        self.bucket()?;
        let images = self
            .host
            .generate_images(ImageGenerateRequest {
                model,
                prompt,
                size,
                count: u32::try_from(count).unwrap_or(1),
            })
            .await
            .map_err(|e| host_sentence("generate the image", &e))?;
        self.save(images, "generate").await
    }

    async fn edit(&self, arguments: &Value) -> Result<Value, String> {
        let model = self.model()?.to_owned();
        let bucket = self.bucket()?.to_owned();
        let prompt = string_arg(arguments, "prompt")
            .ok_or_else(|| "A non-empty `prompt` is required.".to_owned())?;
        let image = string_arg(arguments, "image")
            .ok_or_else(|| "`image` (an artifact filepath) is required.".to_owned())?;
        let (image_bucket, image_key) = split_filepath(&bucket, &image)?;
        upload_name_ok(file_name(image_key))?;
        let mask_path = string_arg(arguments, "mask");
        let mask_target = match mask_path.as_deref() {
            Some(mask) => {
                let (mask_bucket, mask_key) = split_filepath(&bucket, mask)?;
                upload_name_ok(file_name(mask_key))?;
                Some((mask_bucket, mask_key))
            }
            None => None,
        };
        // Refuse a request that would be too large BEFORE buffering either
        // file: the sizes come from the objects' metadata when the host can
        // tell.
        let image_size = self.size_of(image_bucket, image_key, &image).await?;
        let mask_size = match (mask_target, mask_path.as_deref()) {
            (Some((b, k)), Some(path)) => Some(self.size_of(b, k, path).await?),
            _ => None,
        };
        if let (Some(i), Some(Some(m))) = (image_size, mask_size) {
            if i.saturating_add(m) > MAX_IMAGE_EDIT_INPUT_BYTES {
                return Err(combined_too_large(&image, i, mask_path.as_deref(), Some(m)));
            }
        } else if let Some(i) = image_size
            && i > MAX_IMAGE_EDIT_INPUT_BYTES
        {
            return Err(combined_too_large(&image, i, None, None));
        }
        // Sizes unknown (or only one known) stay bounded: the second read may
        // take no more than what the first left of the combined limit.
        let source = self
            .host
            .read_object(
                image_bucket,
                image_key,
                MAX_IMAGE_EDIT_INPUT_BYTES.min(MAX_OFFICE_OBJECT_BYTES),
            )
            .await
            .map_err(|e| Self::read_sentence(&e, &image, None))?;
        let mask = match (mask_target, mask_path.as_deref()) {
            (Some((mask_bucket, mask_key)), Some(path)) => {
                let left = MAX_IMAGE_EDIT_INPUT_BYTES.saturating_sub(source.len() as u64);
                let bytes = self
                    .host
                    .read_object(mask_bucket, mask_key, left)
                    .await
                    .map_err(|e| {
                        Self::read_sentence(&e, path, Some((&image, source.len() as u64)))
                    })?;
                Some(ImageEditInput {
                    name: file_name(mask_key).to_owned(),
                    bytes,
                })
            }
            _ => None,
        };
        let images = self
            .host
            .edit_images(ImageEditRequest {
                model,
                prompt,
                image: ImageEditInput {
                    name: file_name(image_key).to_owned(),
                    bytes: source,
                },
                mask,
            })
            .await
            .map_err(|e| host_sentence("edit the image", &e))?;
        self.save(images, "edit").await
    }

    /// The size of one input, from the host's metadata (`None`: it cannot
    /// tell). A missing file is reported as one.
    async fn size_of(&self, bucket: &str, key: &str, path: &str) -> Result<Option<u64>, String> {
        self.host
            .object_size(bucket, key)
            .await
            .map_err(|e| host_sentence(&format!("read `{path}`"), &e))
    }

    /// A failed read as the model reads it. A read that stopped at the bound
    /// left for the mask is the combined size, and says so.
    fn read_sentence(error: &OfficeHostError, path: &str, image: Option<(&str, u64)>) -> String {
        if error.kind() == OfficeFailure::TooLarge
            && let Some((image_path, image_bytes)) = image
        {
            return format!(
                "The image `{image_path}` ({image_bytes} bytes) and the mask `{path}` together are over the {MAX_IMAGE_EDIT_INPUT_BYTES} bytes one edit accepts. Use smaller files."
            );
        }
        host_sentence(&format!("read `{path}`"), error)
    }
}

/// The sentence for an edit whose inputs are over the combined limit.
fn combined_too_large(
    image: &str,
    image_bytes: u64,
    mask: Option<&str>,
    mask_bytes: Option<u64>,
) -> String {
    match (mask, mask_bytes) {
        (Some(mask), Some(mask_bytes)) => format!(
            "The image `{image}` ({image_bytes} bytes) and the mask `{mask}` ({mask_bytes} bytes) together are {} bytes, over the {MAX_IMAGE_EDIT_INPUT_BYTES} bytes one edit accepts. Use smaller files.",
            image_bytes.saturating_add(mask_bytes)
        ),
        _ => format!(
            "The image `{image}` is {image_bytes} bytes, over the {MAX_IMAGE_EDIT_INPUT_BYTES} bytes one edit accepts. Use a smaller file."
        ),
    }
}

#[async_trait]
impl Tool for ImageGenToolImpl {
    fn name(&self) -> &str {
        self.tool.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        // The SDK groups both under "write": they create artifacts.
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.tool.parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // Every failure is a tool RESULT the model reads, never an error that
        // aborts the turn (the artifact family's rule).
        let outcome = match self.tool {
            ImageGenTool::GenerateImage => self.generate(&arguments).await,
            ImageGenTool::EditImage => self.edit(&arguments).await,
        };
        Ok(outcome.unwrap_or_else(Value::String))
    }
}
