//! The `pptx` toolkit's settings: the SDK's two fields, `bucket_name` and
//! `selected_tools` (`elitea_sdk/tools/pptx/__init__.py`,
//! `toolkit_config_schema`). The parsing is the Office families' shared one
//! (`office_common`).
//!
//! There is no credential: the authority is the live execution claim, lent
//! as the Office host (`office_host`), exactly as for the `artifact` family.

use super::super::office_common::{OfficeConfigError, OfficeConfigErrorCode, OfficeToolkitConfig};
use super::schema::PptxTool;

pub(crate) type PptxConfigErrorCode = OfficeConfigErrorCode;
pub(crate) type PptxConfigError = OfficeConfigError;
/// One invocation-scoped pptx toolkit: its default bucket and the selected
/// tools.
pub(crate) type PptxToolkitConfig = OfficeToolkitConfig<PptxTool>;
