use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_office_core::Limits;
use serde_json::{Value, json};

use crate::toolkits::families::artifact::ArtifactToolAuthority;
use crate::toolkits::families::office_common::{
    OfficeIo, OfficeToolsetError, beside, build_toolset,
};
use crate::toolkits::families::office_host::SharedOfficeToolHost;
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::config::DocxToolkitConfig;
use super::ops::{self, Sentence};
use super::schema::DocxTool;

/// Cap on the text a read returns, in characters (the SDK's own read cap).
const MAX_READ_CHARS: usize = 200_000;
const DOCX_MEDIA_TYPE: &str =
    "application/vnd.openxmlformats-officedocument.wordprocessingml.document";

/// Build the claim-scoped docx toolset. `None` when the authority lends no
/// Office host: the toolkit is then left out as unsupported, never bound to
/// tools that would fail on every call.
pub(crate) fn build_docx_toolset(
    toolkit_name: &str,
    config: &DocxToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    authority: &ArtifactToolAuthority,
) -> Result<Option<BasicToolset>, OfficeToolsetError> {
    let Some(host) = authority.office() else {
        return Ok(None);
    };
    build_with_host(toolkit_name, config, policy, host, Limits::default()).map(Some)
}

pub(crate) fn build_with_host(
    toolkit_name: &str,
    config: &DocxToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    host: &SharedOfficeToolHost,
    limits: Limits,
) -> Result<BasicToolset, OfficeToolsetError> {
    let io = Arc::new(OfficeIo {
        kind: ops::KIND,
        bucket: Arc::from(config.bucket()),
        host: Arc::clone(host),
        limits: Arc::new(limits),
    });
    build_toolset(toolkit_name, config, policy, |tool, description| {
        Arc::new(DocxToolImpl {
            tool,
            io: Arc::clone(&io),
            description,
        })
    })
}

struct DocxToolImpl {
    tool: DocxTool,
    io: Arc<OfficeIo>,
    description: Box<str>,
}

#[async_trait]
impl Tool for DocxToolImpl {
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
        // Two tools editing one document would race on the artifact; reads
        // of one are safe.
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
        // Every failure is a tool RESULT the model reads, never an error that
        // aborts the turn (the artifact family's rule).
        Ok(match self.run(&arguments).await {
            Ok(value) => value,
            Err(message) => Value::String(message),
        })
    }
}

impl DocxToolImpl {
    async fn run(&self, args: &Value) -> Result<Value, Sentence> {
        match self.tool {
            DocxTool::CreateDocument => self.create(args).await,
            DocxTool::ReadDocument => self.read_document(args).await,
            DocxTool::ListPlaceholders => self.list_placeholders(args).await,
            DocxTool::ReplacePlaceholders => self.replace_placeholders(args).await,
            DocxTool::ExportPdf => self.export_pdf(args).await,
            DocxTool::RenderPage => self.render_page(args).await,
            _ => self.edit(args).await,
        }
    }

    async fn edit(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let output = ops::opt_str(args, "output_file_name").unwrap_or_else(|| file.clone());
        let bytes = self.io.read(&file).await?;
        let image = if self.tool == DocxTool::AddImage {
            let name = ops::req_str(args, "image_file_name")?;
            Some(self.io.read(&name).await?)
        } else {
            None
        };
        let (tool, call) = (self.tool, args.clone());
        let (applied, saved) = self
            .io
            .blocking(move |limits| ops::run_edit(tool, &call, &bytes, image.as_deref(), limits))
            .await?;
        let stored = self.io.save(&output, DOCX_MEDIA_TYPE, saved).await?;
        Ok(Value::String(format!(
            "{} Saved {}.",
            applied.message,
            stored.said()
        )))
    }

    async fn create(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let title = ops::opt_str(args, "title");
        let saved = self
            .io
            .blocking(move |limits| ops::create(title.as_deref(), limits))
            .await?;
        // The server refuses a taken name unless the caller asked to replace it.
        let overwrite = ops::opt_bool(args, "overwrite").unwrap_or(false);
        let stored = self
            .io
            .create(&file, DOCX_MEDIA_TYPE, saved, overwrite)
            .await?;
        Ok(Value::String(format!("Created {}.", stored.said())))
    }

    async fn read_document(&self, args: &Value) -> Result<Value, Sentence> {
        let bytes = self.io.read(&ops::req_str(args, "file_name")?).await?;
        let plain = ops::opt_str(args, "format").as_deref() == Some("text");
        let text = self
            .io
            .blocking(move |limits| {
                let doc = ops::open(&bytes, limits)?;
                if plain {
                    doc.to_plain_text()
                } else {
                    doc.to_markdown()
                }
                .map_err(|e| ops::office_sentence(&e))
            })
            .await?;
        if text.chars().count() > MAX_READ_CHARS {
            let cut: String = text.chars().take(MAX_READ_CHARS).collect();
            return Ok(Value::String(format!(
                "{cut}\n\n[Truncated at {MAX_READ_CHARS} characters: the document is longer.]"
            )));
        }
        Ok(Value::String(text))
    }

    async fn list_placeholders(&self, args: &Value) -> Result<Value, Sentence> {
        let bytes = self.io.read(&ops::req_str(args, "file_name")?).await?;
        let all = ops::opt_bool(args, "include_all").unwrap_or(false);
        self.io
            .blocking(move |limits| Ok(ops::list_report(&ops::facts(&bytes, limits)?, all)))
            .await
    }

    async fn replace_placeholders(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let output = ops::opt_str(args, "output_file_name").unwrap_or_else(|| file.clone());
        let bytes = self.io.read(&file).await?;
        let (tool, call) = (self.tool, args.clone());
        let (applied, saved) = self
            .io
            .blocking(move |limits| ops::run_edit(tool, &call, &bytes, None, limits))
            .await?;
        let stored = self.io.save(&output, DOCX_MEDIA_TYPE, saved).await?;
        let (total, not_found) = ops::replace_report(&applied);
        Ok(json!({
            "status": "success",
            "message": format!("Replaced text in {total} place{}. Saved {}.", if total == 1 { "" } else { "s" }, stored.said()),
            "file": stored.key,
            "bucket": stored.bucket,
            "replacements_applied": total,
            "keys_not_found": not_found,
        }))
    }

    async fn export_pdf(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let bytes = self.io.read(&file).await?;
        let pdf = self
            .io
            .blocking(move |limits| {
                ops::open(&bytes, limits)?
                    .export_pdf()
                    .map_err(|e| ops::office_sentence(&e))
            })
            .await?;
        let output = ops::opt_str(args, "output_file_name")
            .unwrap_or_else(|| beside(&self.io.bucket, &file, ".pdf"));
        let stored = self.io.save(&output, "application/pdf", pdf).await?;
        Ok(Value::String(format!(
            "Exported {}. The export is approximate: charts may be drawn as placeholders and fonts that are not bundled render with a substitute.",
            stored.said()
        )))
    }

    async fn render_page(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let bytes = self.io.read(&file).await?;
        let page = ops::opt_uint(args, "page").unwrap_or(1);
        if page == 0 {
            return Err("`page` is a page number from 1.".to_owned());
        }
        let dpi = ops::opt_uint(args, "dpi").unwrap_or(100);
        if !(36..=300).contains(&dpi) {
            return Err("`dpi` must be 36 to 300.".to_owned());
        }
        let index = usize::try_from(page - 1).unwrap_or(usize::MAX);
        #[allow(clippy::cast_precision_loss, reason = "a dpi of at most 300")]
        let png = self
            .io
            .blocking(move |limits| {
                ops::open(&bytes, limits)?
                    .render_page_png(index, dpi as f64)
                    .map_err(|e| match e {
                        elitea_office_core::OfficeError::NotFound(_) => {
                            format!("Page {page} does not exist: the document ends before it.")
                        }
                        other => ops::office_sentence(&other),
                    })
            })
            .await?;
        let output = ops::opt_str(args, "output_file_name")
            .unwrap_or_else(|| beside(&self.io.bucket, &file, &format!("-page-{page}.png")));
        let stored = self.io.save(&output, "image/png", png).await?;
        Ok(json!({
            "status": "success",
            "page": page,
            "image": stored.key,
            "bucket": stored.bucket,
            "bytes": stored.bytes,
            "note": "Approximate rendering: charts may be drawn as placeholders and fonts that are not bundled render with a substitute.",
        }))
    }
}
