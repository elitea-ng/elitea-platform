use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_office_core::Limits;
use serde_json::Value;

use crate::toolkits::families::artifact::ArtifactToolAuthority;
use crate::toolkits::families::office_common::{OfficeIo, OfficeToolsetError, build_toolset};
use crate::toolkits::families::office_host::SharedOfficeToolHost;
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::config::XlsxToolkitConfig;
use super::ops::{self, Sentence};
use super::schema::XlsxTool;

const XLSX_MEDIA_TYPE: &str = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet";

/// Build the claim-scoped xlsx toolset. `None` when the authority lends no
/// Office host: the toolkit is then left out as unsupported, never bound to
/// tools that would fail on every call.
pub(crate) fn build_xlsx_toolset(
    toolkit_name: &str,
    config: &XlsxToolkitConfig,
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
    config: &XlsxToolkitConfig,
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
        Arc::new(XlsxToolImpl {
            tool,
            io: Arc::clone(&io),
            description,
        })
    })
}

struct XlsxToolImpl {
    tool: XlsxTool,
    io: Arc<OfficeIo>,
    description: Box<str>,
}

#[async_trait]
impl Tool for XlsxToolImpl {
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
        // Two tools editing one workbook would race on the artifact; reads of
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
        // Every failure is a tool RESULT the model reads, never an error that
        // aborts the turn (the artifact family's rule).
        Ok(match self.run(&arguments).await {
            Ok(value) => value,
            Err(message) => Value::String(message),
        })
    }
}

impl XlsxToolImpl {
    async fn run(&self, args: &Value) -> Result<Value, Sentence> {
        match self.tool {
            XlsxTool::CreateWorkbook => self.create(args).await,
            XlsxTool::ListSheets => {
                let bytes = self.io.read(&ops::req_str(args, "file_name")?).await?;
                self.io
                    .blocking(move |limits| ops::list_sheets(&bytes, limits))
                    .await
            }
            XlsxTool::ReadRange | XlsxTool::SheetToMarkdown => self.read_range(args).await,
            _ => self.edit(args).await,
        }
    }

    async fn edit(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let output = ops::opt_str(args, "output_file_name").unwrap_or_else(|| file.clone());
        let bytes = self.io.read(&file).await?;
        let (tool, call) = (self.tool, args.clone());
        let (message, saved) = self
            .io
            .blocking(move |limits| ops::run_edit(tool, &call, &bytes, limits))
            .await?;
        let stored = self.io.save(&output, XLSX_MEDIA_TYPE, saved).await?;
        Ok(Value::String(format!("{message} Saved {}.", stored.said())))
    }

    async fn create(&self, args: &Value) -> Result<Value, Sentence> {
        let file = ops::req_str(args, "file_name")?;
        let sheet = ops::opt_str(args, "sheet_name");
        let saved = self
            .io
            .blocking(move |limits| ops::create(sheet.as_deref(), limits))
            .await?;
        // The server refuses a taken name unless the caller asked to replace it.
        let overwrite = ops::opt_bool(args, "overwrite").unwrap_or(false);
        let stored = self
            .io
            .create(&file, XLSX_MEDIA_TYPE, saved, overwrite)
            .await?;
        Ok(Value::String(format!("Created {}.", stored.said())))
    }

    async fn read_range(&self, args: &Value) -> Result<Value, Sentence> {
        let bytes = self.io.read(&ops::req_str(args, "file_name")?).await?;
        let json = self.tool == XlsxTool::ReadRange
            && ops::opt_str(args, "format").as_deref() == Some("json");
        // `sheet_to_markdown` takes no range and no format.
        let mut call = args.clone();
        if self.tool == XlsxTool::SheetToMarkdown
            && let Some(map) = call.as_object_mut()
        {
            map.remove("range");
        }
        self.io
            .blocking(move |limits| ops::read_range(&bytes, &call, limits, json))
            .await
    }
}
