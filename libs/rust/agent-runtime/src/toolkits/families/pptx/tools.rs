use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_office_core::{Deck, Limits};
use serde_json::{Map, Value, json};

use crate::toolkits::families::artifact::ArtifactToolAuthority;
use crate::toolkits::families::office_host::{
    MAX_OFFICE_OBJECT_BYTES, OfficeFailure, OfficeHostError, OfficeModelRequest,
    SharedOfficeToolHost,
};
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::config::{PptxConfigError, PptxConfigErrorCode, PptxToolkitConfig};
use super::model;
use super::ops::{self, Sentence};
use super::schema::PptxTool;

const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_FILE_NAME_BYTES: usize = 1_024;
const DEFAULT_BATCH_SIZE: usize = 20;
/// The most unique placeholders or paragraphs one call sends to the model.
/// With the batch cap it bounds the model calls a single tool call can make
/// (`MAX_MODEL_ITEMS / batch size`, 2000 / 20 = 100 by default).
const MAX_MODEL_ITEMS: usize = 2_000;
const TRANSLATE_BATCH: usize = 20;
/// How long a call waits for a free document slot: up to 8 retries at
/// 250 ms, 500 ms, ... (9 s in all), under the engine's 20 s call deadline.
const BUSY_RETRIES: u32 = 8;
const BUSY_BACKOFF: std::time::Duration = std::time::Duration::from_millis(250);
/// The slides sent to the model as visual context when `pdf_file_name` is
/// given (the SDK sends at most three PDF pages).
const MAX_CONTEXT_IMAGES: usize = 3;
/// Cap on the Markdown a read returns: the SDK's own read cap in characters.
const MAX_MARKDOWN_CHARS: usize = 200_000;
const PPTX_MEDIA_TYPE: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.presentation";

/// Stable family-toolset construction failure category.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PptxToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    InvalidDefinition,
}

pub(crate) struct PptxToolsetError {
    code: PptxToolsetErrorCode,
}

impl PptxToolsetError {
    pub(crate) const fn code(&self) -> PptxToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for PptxToolsetError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PptxToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for PptxToolsetError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self.code {
            PptxToolsetErrorCode::InvalidConfiguration => {
                "the pptx toolkit configuration is invalid"
            }
            PptxToolsetErrorCode::ResourceExhausted => {
                "the pptx toolkit configuration exceeds its approved limit"
            }
            PptxToolsetErrorCode::InvalidDefinition => "the pptx ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for PptxToolsetError {}

impl From<PptxConfigError> for PptxToolsetError {
    fn from(source: PptxConfigError) -> Self {
        Self {
            code: match source.code() {
                PptxConfigErrorCode::InvalidConfiguration => {
                    PptxToolsetErrorCode::InvalidConfiguration
                }
                PptxConfigErrorCode::ResourceExhausted => PptxToolsetErrorCode::ResourceExhausted,
            },
        }
    }
}

impl From<MaterializedToolsetError> for PptxToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: PptxToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the claim-scoped pptx toolset. `None` when the authority lends no
/// Office host: the toolkit is then left out as unsupported, never bound to
/// tools that would fail on every call.
pub(crate) fn build_pptx_toolset(
    toolkit_name: &str,
    config: &PptxToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    authority: &ArtifactToolAuthority,
) -> Result<Option<BasicToolset>, PptxToolsetError> {
    let Some(host) = authority.office() else {
        return Ok(None);
    };
    build_with_host(toolkit_name, config, policy, host, Limits::default()).map(Some)
}

pub(crate) fn build_with_host(
    toolkit_name: &str,
    config: &PptxToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    host: &SharedOfficeToolHost,
    limits: Limits,
) -> Result<BasicToolset, PptxToolsetError> {
    let bucket: Arc<str> = Arc::from(config.bucket());
    let limits = Arc::new(limits);
    let tools: Vec<Arc<dyn Tool>> = config
        .tools()
        .into_iter()
        .map(|tool| -> Arc<dyn Tool> {
            Arc::new(PptxToolImpl {
                tool,
                bucket: Arc::clone(&bucket),
                host: Arc::clone(host),
                limits: Arc::clone(&limits),
                description: tool_description(toolkit_name, tool.description()),
            })
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "pptx", policy, tools).map_err(Into::into)
}

/// The SDK's description rule: `Toolkit: <name>\n` before the description,
/// cut at 1000 characters.
fn tool_description(toolkit_name: &str, text: &str) -> Box<str> {
    format!("Toolkit: {toolkit_name}\n{text}")
        .chars()
        .take(MAX_DESCRIPTION_BYTES)
        .collect::<String>()
        .into_boxed_str()
}

struct PptxToolImpl {
    tool: PptxTool,
    bucket: Arc<str>,
    host: SharedOfficeToolHost,
    limits: Arc<Limits>,
    description: Box<str>,
}

#[async_trait]
impl Tool for PptxToolImpl {
    fn name(&self) -> &str {
        self.tool.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.tool.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        // Two tools editing one deck would race on the artifact; reads of
        // one are safe.
        self.tool.is_read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.tool.parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // Every failure is a tool RESULT the model reads, never an error
        // that aborts the turn (the artifact family's rule). The two SDK
        // tools keep the SDK's {"status": ..., "message": ...} shape.
        let outcome = self.run(&arguments).await;
        Ok(match (self.tool, outcome) {
            (_, Ok(value)) => value,
            (PptxTool::FillTemplate, Err(message)) => json!({
                "status": "error",
                "message": format!("Failed to fill template: {message}"),
            }),
            (PptxTool::TranslatePresentation, Err(message)) => json!({
                "status": "error",
                "message": format!("Failed to translate presentation: {message}"),
            }),
            (_, Err(message)) => Value::String(message),
        })
    }
}

/// A host failure as the model reads it: one sentence per real cause.
pub(crate) fn host_sentence(action: &str, error: &OfficeHostError) -> Sentence {
    tracing::warn!(
        event = "pptx_tool_host_failed",
        reason_code = error.reason_code(),
        action,
        "a pptx tool's platform call failed"
    );
    match error.kind() {
        OfficeFailure::NotFound => {
            format!("Could not {action}: no such file in the bucket. Check the file name.")
        }
        OfficeFailure::AccessRefused => format!(
            "Could not {action}: the platform refused access to the file store. Do not retry; tell the user."
        ),
        OfficeFailure::AlreadyExists => format!(
            "Could not {action}: a file with that name already exists. Choose another name, or ask to replace it."
        ),
        OfficeFailure::UnsupportedType => format!(
            "Could not {action}: the file store does not accept that type of file (a .pptx or an image is expected)."
        ),
        OfficeFailure::InvalidRequest => format!(
            "Could not {action}: the file name or bucket is not valid, or the request was refused as invalid."
        ),
        OfficeFailure::TooLarge => {
            format!("Could not {action}: the file is larger than the platform accepts.")
        }
        OfficeFailure::BudgetRefused => format!(
            "Could not {action}: the model budget is exhausted. Do not retry; tell the user."
        ),
        OfficeFailure::NoModel => format!(
            "Could not {action}: this tool needs a chat model and none is bound in this run (a pipeline node binds none). Use list_placeholders and replace_placeholders with your own content, or run it from an agent."
        ),
        OfficeFailure::RateLimited => format!(
            "Could not {action}: the model gateway is rate-limiting this call. Try again shortly."
        ),
        OfficeFailure::Timeout => {
            format!("Could not {action}: the model call timed out. It may work if tried again.")
        }
        OfficeFailure::Conflict => format!(
            "Could not {action}: the model gateway reported a conflict. It may work if tried again."
        ),
        OfficeFailure::Unreachable => format!(
            "Could not {action}: the platform could not be reached. Tell the user it did not happen."
        ),
    }
}

/// The key a file is stored under: what the tool accepts as `file_name`. A
/// leading slash is dropped, and so is a leading `/{bucket}/` of the
/// toolkit's own bucket, so the path form the platform shows for a file works
/// as the next call's name.
fn object_key(bucket: &str, name: &str) -> Result<String, Sentence> {
    let trimmed = name.trim();
    let key = if let Some(rooted) = trimmed.strip_prefix('/') {
        let rooted = rooted.trim_start_matches('/');
        rooted
            .strip_prefix(bucket)
            .and_then(|rest| rest.strip_prefix('/'))
            .unwrap_or(rooted)
    } else {
        trimmed
    };
    if key.is_empty() || key.len() > MAX_FILE_NAME_BYTES {
        return Err("The file name is empty or too long.".to_owned());
    }
    if key.split('/').any(|segment| segment == "..") {
        return Err("The file name may not contain `..`.".to_owned());
    }
    Ok(key.to_owned())
}

/// A file the tool stored: the bucket and the key (the name every tool of the
/// family accepts), named separately.
struct Stored {
    bucket: String,
    key: String,
    bytes: u64,
}

impl Stored {
    fn said(&self) -> String {
        format!(
            "`{}` in bucket `{}` ({} bytes)",
            self.key, self.bucket, self.bytes
        )
    }
}

/// The directory prefix of a key, with its trailing slash (empty for a file at
/// the bucket's root): where an output with no name of its own goes, next to
/// the source.
fn directory_of(key: &str) -> &str {
    key.rfind('/').map_or("", |i| &key[..=i])
}

fn stem(name: &str) -> &str {
    let file = name.rsplit('/').next().unwrap_or(name);
    file.rsplit_once('.').map_or(file, |(stem, _)| stem)
}

impl PptxToolImpl {
    async fn read(&self, name: &str) -> Result<Vec<u8>, Sentence> {
        let key = object_key(&self.bucket, name)?;
        self.host
            .read_object(&self.bucket, &key, MAX_OFFICE_OBJECT_BYTES)
            .await
            .map_err(|e| host_sentence(&format!("read `{key}`"), &e))
    }

    /// Store `bytes` and report where. `overwrite` is false only for a file
    /// the caller must not replace by accident (`create_presentation`); every
    /// other output is regenerated or an edit in place, and replaces.
    async fn write(
        &self,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<Stored, OfficeHostError> {
        let key = object_key(&self.bucket, name).map_err(|_| {
            OfficeHostError::new(OfficeFailure::InvalidRequest, "office_host.invalid_name")
        })?;
        let written = self
            .host
            .write_object(&self.bucket, &key, media_type, bytes, overwrite)
            .await?;
        Ok(Stored {
            bucket: written.bucket,
            key: written.name,
            bytes: written.byte_length,
        })
    }

    /// `write`, the failure as the sentence the model reads.
    async fn save(&self, name: &str, media_type: &str, bytes: Vec<u8>) -> Result<Stored, Sentence> {
        self.write(name, media_type, bytes, true)
            .await
            .map_err(|e| host_sentence(&format!("save `{name}`"), &e))
    }

    /// Run blocking document work off the async workers.
    ///
    /// The engine admits a few documents at a time per process
    /// (`Limits::max_concurrent`, ADR-0032 decision 3) and answers `Busy`
    /// at once rather than queueing; a call waits its turn here instead, a
    /// short while, before it tells the model to try again. `work` runs once
    /// per attempt and borrows what it needs.
    async fn blocking<T, F>(&self, work: F) -> Result<T, Sentence>
    where
        T: Send + 'static,
        F: Fn(&Limits) -> Result<T, Sentence> + Send + Sync + 'static,
    {
        let work = Arc::new(work);
        let mut attempt = 0u32;
        loop {
            let (limits, run) = (Arc::clone(&self.limits), Arc::clone(&work));
            let outcome = tokio::task::spawn_blocking(move || run(&limits))
                .await
                .map_err(|_| "The document engine stopped unexpectedly on this file.".to_owned())?;
            match outcome {
                Err(sentence) if sentence == ops::BUSY && attempt < BUSY_RETRIES => {
                    attempt += 1;
                    tokio::time::sleep(BUSY_BACKOFF * attempt).await;
                }
                other => return other,
            }
        }
    }

    async fn run(&self, args: &Value) -> Result<Value, Sentence> {
        match self.tool {
            PptxTool::FillTemplate => self.fill_template(args).await,
            PptxTool::TranslatePresentation => self.translate(args).await,
            PptxTool::ListPlaceholders => self.list_placeholders(args).await,
            PptxTool::ReplacePlaceholders => self.replace_placeholders(args).await,
            PptxTool::CreatePresentation => self.create(args).await,
            PptxTool::RenderPreview => self.render_preview(args).await,
            PptxTool::DesignCheck => self.design_check(args).await,
            PptxTool::ToMarkdown => self.to_markdown(args).await,
            PptxTool::ExportPdf => self.export_pdf(args).await,
            _ => self.edit(args).await,
        }
    }

    // -- Authoring ----------------------------------------------------------

    async fn edit(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let output = ops::opt_str(args, "output_file_name").unwrap_or_else(|| file.clone());
        let bytes = self.read(&file).await?;
        let image = if self.tool == PptxTool::AddImage {
            let name = ops::req_str(args, "image_file_name")?;
            Some(self.read(&name).await?)
        } else {
            None
        };
        let (tool, call) = (self.tool, args.clone());
        let (message, saved) = self
            .blocking(move |limits| ops::run_edit(tool, &call, &bytes, image.as_deref(), limits))
            .await?;
        let stored = self.save(&output, PPTX_MEDIA_TYPE, saved).await?;
        Ok(Value::String(format!("{message} Saved {}.", stored.said())))
    }

    async fn create(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let (title, subtitle) = (ops::opt_str(args, "title"), ops::opt_str(args, "subtitle"));
        let saved = self
            .blocking(move |limits| {
                let mut deck = Deck::new(limits);
                // A deck with no slide at all is saved without a slide list,
                // which the slide library cannot add to after a reopen (the
                // added slide is dropped on save). One slide always exists.
                let layout = if title.is_some() || subtitle.is_some() {
                    elitea_office_core::SlideLayout::Title
                } else {
                    elitea_office_core::SlideLayout::Blank
                };
                let slide = deck
                    .add_slide(layout)
                    .map_err(|e| ops::office_sentence(&e))?;
                {
                    if let Some(title) = &title {
                        deck.set_title(slide, title)
                            .map_err(|e| ops::office_sentence(&e))?;
                    }
                    if let Some(subtitle) = &subtitle {
                        deck.add_text(
                            slide,
                            subtitle,
                            elitea_office_core::Rect::inches(1.0, 4.0, 8.0, 1.0),
                        )
                        .map_err(|e| ops::office_sentence(&e))?;
                    }
                }
                ops::save(&deck)
            })
            .await?;
        // The server refuses a taken name unless the caller asked to replace it.
        let overwrite = ops::opt_bool(args, "overwrite").unwrap_or(false);
        let stored = self
            .write(&file, PPTX_MEDIA_TYPE, saved, overwrite)
            .await
            .map_err(|e| {
                if e.kind() == OfficeFailure::AlreadyExists {
                    format!("`{file}` already exists. Pass overwrite=true to replace it, or choose another name.")
                } else {
                    host_sentence(&format!("create `{file}`"), &e)
                }
            })?;
        Ok(Value::String(format!("Created {}.", stored.said())))
    }

    // -- Deterministic halves ------------------------------------------------

    async fn list_placeholders(&self, args: &Value) -> Result<Value, Sentence> {
        let bytes = self.read(&ops::req_str(args, "file_name")?).await?;
        let all = ops::opt_bool(args, "include_all").unwrap_or(false);
        self.blocking(move |limits| {
            let opened = ops::open_with_facts(&bytes, limits)?;
            ops::list_report(&opened.deck, &opened.facts, all)
        })
        .await
    }

    async fn replace_placeholders(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let output = ops::opt_str(args, "output_file_name").unwrap_or_else(|| file.clone());
        let replacements: BTreeMap<String, String> = args
            .get("replacements")
            .and_then(Value::as_object)
            .ok_or_else(|| {
                "`replacements` is required: a map from text to replacement.".to_owned()
            })?
            .iter()
            .map(|(k, v)| {
                (
                    k.clone(),
                    v.as_str().map_or_else(|| v.to_string(), str::to_owned),
                )
            })
            .collect();
        if replacements.is_empty() || replacements.keys().any(String::is_empty) {
            return Err("`replacements` needs at least one non-empty key.".to_owned());
        }
        if replacements.len() > 500 {
            return Err("Too many replacements in one call (500 at most).".to_owned());
        }
        let bytes = self.read(&file).await?;
        let (changed, unmatched, saved) = self
            .blocking(move |limits| ops::run_replace(&bytes, &replacements, limits))
            .await?;
        let stored = self.save(&output, PPTX_MEDIA_TYPE, saved).await?;
        Ok(json!({
            "status": "success",
            "message": format!("Replaced text in {changed} place{}. Saved {}.", if changed == 1 { "" } else { "s" }, stored.said()),
            "file": stored.key,
            "bucket": stored.bucket,
            "replacements_applied": changed,
            "keys_not_found": unmatched,
        }))
    }

    // -- Reads -----------------------------------------------------------------

    async fn to_markdown(&self, args: &Value) -> Result<Value, Sentence> {
        let bytes = self.read(&ops::req_str(args, "file_name")?).await?;
        let markdown = self
            .blocking(move |limits| {
                ops::open(&bytes, limits)?
                    .to_markdown()
                    .map_err(|e| ops::office_sentence(&e))
            })
            .await?;
        if markdown.chars().count() > MAX_MARKDOWN_CHARS {
            let cut: String = markdown.chars().take(MAX_MARKDOWN_CHARS).collect();
            return Ok(Value::String(format!(
                "{cut}\n\n[Truncated at {MAX_MARKDOWN_CHARS} characters: the deck is longer.]"
            )));
        }
        Ok(Value::String(markdown))
    }

    async fn design_check(&self, args: &Value) -> Result<Value, Sentence> {
        let bytes = self.read(&ops::req_str(args, "file_name")?).await?;
        let wanted = args.get("slide").and_then(Value::as_u64);
        self.blocking(move |limits| {
            // Read from the package: nothing here needs the slide library.
            let facts = elitea_office_core::facts_of_package(&bytes, limits)
                .map_err(|e| ops::office_sentence(&e))?;
            let only = match wanted {
                Some(n) => Some(ops::slide_index(n, facts.len())?),
                None => None,
            };
            Ok(ops::design_from_facts(&facts, only))
        })
        .await
    }

    async fn render_preview(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let bytes = self.read(&file).await?;
        let wanted = args.get("slide").and_then(Value::as_u64).unwrap_or(1);
        let (png, charts, slide) = self
            .blocking(move |limits| {
                let opened = ops::open_with_facts(&bytes, limits)?;
                let slide = ops::slide_index(wanted, opened.facts.len())?;
                let png = opened
                    .deck
                    .render_preview(slide)
                    .map_err(|e| ops::office_sentence(&e))?;
                let charts = ops::chart_data(&opened.facts[slide]);
                Ok((png, charts, slide))
            })
            .await?;
        // With no name of its own the preview goes next to the deck.
        let output = ops::opt_str(args, "output_file_name").unwrap_or_else(|| {
            let source = object_key(&self.bucket, &file).unwrap_or_else(|_| file.clone());
            format!(
                "{}{}-slide-{}.png",
                directory_of(&source),
                stem(&source),
                slide + 1
            )
        });
        let stored = self.save(&output, "image/png", png).await?;
        let mut note = String::from(
            "Approximate preview: charts are drawn as grey placeholders (their data is in `charts`), SVG pictures are not drawn, and fonts that are not bundled render with a substitute.",
        );
        if charts.is_empty() {
            note = note.replace(
                "charts are drawn as grey placeholders (their data is in `charts`), ",
                "",
            );
        }
        Ok(json!({
            "status": "success",
            "slide": slide + 1,
            "image": stored.key,
            "bucket": stored.bucket,
            "bytes": stored.bytes,
            "charts": charts,
            "note": note,
        }))
    }

    async fn export_pdf(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let bytes = self.read(&file).await?;
        let pdf = self
            .blocking(move |limits| {
                ops::open(&bytes, limits)?
                    .export_pdf()
                    .map_err(|e| ops::office_sentence(&e))
            })
            .await?;
        let output = ops::opt_str(args, "output_file_name").unwrap_or_else(|| {
            let source = object_key(&self.bucket, &file).unwrap_or_else(|_| file.clone());
            format!("{}{}.pdf", directory_of(&source), stem(&source))
        });
        let stored = self.save(&output, "application/pdf", pdf).await?;
        Ok(Value::String(format!(
            "Exported {}. The export is approximate: charts are drawn as placeholders and fonts that are not bundled render with a substitute.",
            stored.said()
        )))
    }

    // -- Model-calling tools -------------------------------------------------

    /// One model call, as the model-facing sentence on failure.
    async fn ask(
        &self,
        prompt: String,
        images_png: std::sync::Arc<[Vec<u8>]>,
    ) -> Result<String, Sentence> {
        self.host
            .complete(OfficeModelRequest { prompt, images_png })
            .await
            .map_err(|e| host_sentence("call the model", &e))
    }

    #[allow(clippy::too_many_lines)]
    async fn fill_template(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let output = ops::req_str(args, "output_file_name")?;
        let description = ops::req_str(args, "content_description")?;
        let batch_size = args
            .get("batch_size")
            .and_then(Value::as_u64)
            .map_or(DEFAULT_BATCH_SIZE, |n| {
                usize::try_from(n).unwrap_or(DEFAULT_BATCH_SIZE)
            })
            .clamp(1, 100);
        let with_pdf = ops::opt_str(args, "pdf_file_name").is_some();
        let bytes = self.read(&file).await?;

        let scan = bytes.clone();
        let (texts, total_locations, slide_count, images) = self
            .blocking(move |limits| {
                let opened = ops::open_with_facts(&scan, limits)?;
                let deck = &opened.deck;
                let locations = ops::placeholder_locations(deck, &opened.facts)?;
                // Deduplicated here, on the blocking pool, and refused as soon
                // as the distinct count passes the cap.
                let unique = ops::distinct_texts(locations.iter().map(|l| l.text.as_str()), MAX_MODEL_ITEMS)
                    .ok_or_else(|| {
                        format!("The template has more than {MAX_MODEL_ITEMS} distinct placeholders; at most {MAX_MODEL_ITEMS} can be filled in one call.")
                    })?;
                // The SDK sends up to three pages of a matching PDF. This
                // runtime has no PDF rasteriser, so when a PDF is named the
                // template's own slides stand in for its pages.
                let mut images = Vec::new();
                if with_pdf && !locations.is_empty() {
                    let mut slides: Vec<usize> = locations.iter().map(|l| l.slide).collect();
                    slides.sort_unstable();
                    slides.dedup();
                    for slide in slides.into_iter().take(MAX_CONTEXT_IMAGES) {
                        if let Ok(png) = deck.render_preview(slide) {
                            images.push(png);
                        }
                    }
                }
                Ok((unique, locations.len(), ops::slide_count(deck)?, std::sync::Arc::<[Vec<u8>]>::from(images)))
            })
            .await?;
        if total_locations == 0 {
            return Ok(json!({
                "status": "warning",
                "message": "No placeholders found in the presentation. Nothing to fill.",
                "url": Value::Null,
            }));
        }
        let mut content: std::collections::HashMap<String, String> =
            std::collections::HashMap::new();
        let batches = texts.len().div_ceil(batch_size);
        let no_images: std::sync::Arc<[Vec<u8>]> = std::sync::Arc::from(Vec::new());
        for (n, batch) in texts.chunks(batch_size).enumerate() {
            // The slide images go with the first batch only: they are the same
            // for every batch, so sending them again would only repeat their
            // tokens. Later batches say they were given.
            let (prompt, sent) = if n == 0 || images.is_empty() {
                (model::fill_prompt(batch, &description), images.clone())
            } else {
                (
                    format!(
                        "{}\n\n(The images of the template's slides were given with the first batch of this request.)",
                        model::fill_prompt(batch, &description)
                    ),
                    std::sync::Arc::clone(&no_images),
                )
            };
            let reply = self.ask(prompt, sent).await?;
            // Only this batch's own keys are taken: a key the model returns
            // that belongs to another batch (or to none) never overwrites.
            let mut parsed = model::parse_fill_reply(&reply, batch);
            for key in batch {
                if let Some(value) = parsed.remove(key) {
                    content.insert(key.clone(), value);
                }
            }
        }

        let content = Arc::new(content);
        let applied = Arc::clone(&content);
        let (text_frame_updates, table_updates, skipped, saved) = self
            .blocking(move |limits| {
                let mut opened = ops::open_with_facts(&bytes, limits)?;
                let (mut frames, mut cells, mut skipped) = (0usize, 0usize, 0usize);
                for location in ops::placeholder_locations(&opened.deck, &opened.facts)? {
                    let text = applied
                        .get(&location.text)
                        .cloned()
                        .unwrap_or_else(|| format!("[Missing content for: {}]", location.text));
                    if ops::write_location(&mut opened.deck, &location, &text)? {
                        match location.place {
                            ops::Place::Frame(_) => frames += 1,
                            ops::Place::Cell { .. } => cells += 1,
                        }
                    } else {
                        skipped += 1;
                    }
                }
                let saved = ops::finish_edit(&opened, limits, &ops::Check::default())?;
                Ok((frames, cells, skipped, saved))
            })
            .await?;
        let stored = self.save(&output, PPTX_MEDIA_TYPE, saved).await?;
        let mut stats = Map::new();
        stats.insert("unique_placeholders".into(), texts.len().into());
        stats.insert("total_placeholder_locations".into(), total_locations.into());
        stats.insert("text_frame_updates".into(), text_frame_updates.into());
        stats.insert("table_updates".into(), table_updates.into());
        // SmartArt text is not edited by this runtime (ADR-0032).
        stats.insert("smartart_updates".into(), 0.into());
        stats.insert("total_slides".into(), slide_count.into());
        stats.insert("batches_processed".into(), batches.into());
        if skipped > 0 {
            stats.insert("locations_skipped".into(), skipped.into());
        }
        if !images.is_empty() {
            stats.insert("visual_context_slides".into(), images.len().into());
        }
        Ok(json!({
            "status": "success",
            "message": format!(
                "Successfully filled {} unique placeholders ({total_locations} total locations) across {slide_count} slides. Saved as {output}",
                texts.len()
            ),
            "url": stored.key,
            "bucket": stored.bucket,
            "stats": Value::Object(stats),
        }))
    }

    async fn translate(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let output = ops::req_str(args, "output_file_name")?;
        let language = model::language_name(&ops::req_str(args, "target_language")?);
        let bytes = self.read(&file).await?;

        let scan = bytes.clone();
        // Collected, deduplicated and capped on the blocking pool.
        let items = self
            .blocking(move |limits| {
                let opened = ops::open_with_facts(&scan, limits)?;
                ops::translation_items(&opened.deck, &opened.facts, MAX_MODEL_ITEMS)?.ok_or_else(
                    || {
                        format!("The deck has more than {MAX_MODEL_ITEMS} distinct paragraphs; at most {MAX_MODEL_ITEMS} can be translated in one call.")
                    },
                )
            })
            .await?;
        let unique = ops::distinct_texts(items.iter().map(|i| i.text.as_str()), usize::MAX)
            .unwrap_or_default();
        let mut translated: std::collections::HashMap<String, String> =
            std::collections::HashMap::new();
        let mut start = 0usize;
        let mut pending: Vec<String> = Vec::new();
        for batch in unique.chunks(TRANSLATE_BATCH) {
            let reply = self
                .ask(
                    model::translate_prompt(batch, &language, start),
                    std::sync::Arc::from(Vec::new()),
                )
                .await?;
            for (text, done) in batch
                .iter()
                .zip(model::parse_translation_reply(&reply, batch, start))
            {
                match done {
                    Some(done) => {
                        translated.insert(text.clone(), done);
                    }
                    None => pending.push(text.clone()),
                }
            }
            start += batch.len();
        }
        // What the model left out is asked for once more; anything still
        // missing keeps its original text and is reported.
        for batch in std::mem::take(&mut pending).chunks(TRANSLATE_BATCH) {
            let reply = self
                .ask(
                    model::translate_prompt(batch, &language, start),
                    std::sync::Arc::from(Vec::new()),
                )
                .await?;
            for (text, done) in batch
                .iter()
                .zip(model::parse_translation_reply(&reply, batch, start))
            {
                if let Some(done) = done {
                    translated.insert(text.clone(), done);
                }
            }
            start += batch.len();
        }
        let ordered: Vec<Option<String>> = items
            .iter()
            .map(|i| translated.get(&i.text).cloned())
            .collect();
        let untranslated = ordered.iter().filter(|t| t.is_none()).count();
        let total = ordered.len();
        let saved = self
            .blocking(move |limits| {
                let mut opened = ops::open_with_facts(&bytes, limits)?;
                // Items were collected on this same file, so they address
                // the same paragraphs.
                ops::apply_translations(&mut opened.deck, &opened.facts, &items, &ordered)?;
                ops::finish_edit(&opened, limits, &ops::Check::default())
            })
            .await?;
        let stored = self.save(&output, PPTX_MEDIA_TYPE, saved).await?;
        if untranslated > 0 {
            return Ok(json!({
                "status": "partial",
                "message": format!(
                    "Partially translated presentation to {language} and saved as {output}: {untranslated} of {total} paragraphs could not be translated and keep their original text."
                ),
                "url": stored.key,
                "bucket": stored.bucket,
                "untranslated_paragraphs": untranslated,
                "translated_paragraphs": total - untranslated,
            }));
        }
        Ok(json!({
            "status": "success",
            "message": format!("Successfully translated presentation to {language} and saved as {output}"),
            "url": stored.key,
            "bucket": stored.bucket,
        }))
    }
}
