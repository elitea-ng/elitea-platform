//! The `docx` toolkit's settings: `bucket_name` and `selected_tools`, the SDK
//! `pptx` toolkit's two fields (the SDK has no `docx` toolkit). The parsing is
//! the Office families' shared one (`office_common`).

use super::super::office_common::OfficeToolkitConfig;
use super::schema::DocxTool;

/// One invocation-scoped docx toolkit: its default bucket and the selected
/// tools.
pub(crate) type DocxToolkitConfig = OfficeToolkitConfig<DocxTool>;
