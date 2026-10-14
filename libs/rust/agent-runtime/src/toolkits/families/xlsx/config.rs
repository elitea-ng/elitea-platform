//! The `xlsx` toolkit's settings: `bucket_name` and `selected_tools`, the SDK
//! `pptx` toolkit's two fields (the SDK has no `xlsx` toolkit). The parsing is
//! the Office families' shared one (`office_common`).

use super::super::office_common::OfficeToolkitConfig;
use super::schema::XlsxTool;

/// One invocation-scoped xlsx toolkit: its default bucket and the selected
/// tools.
pub(crate) type XlsxToolkitConfig = OfficeToolkitConfig<XlsxTool>;
