use serde_json::{Map, Value};

use crate::toolkits::families::ado::config::{AdoConfigError, AdoToolkitConfig};
use crate::toolkits::families::ado::image_defaults::AdoImageDefaults;

/// `ado_boards` settings (`tools/ado/work_item/__init__.py::get_toolkit`):
/// `ado_configuration`, `project`, `limit` (default 5) and `selected_tools`.
/// The embedding setting only feeds the index tools, which this runtime does
/// not serve (the SDK's `pgvector_configuration` is retired and never sent).
/// `process_images` and `image_description_prompt` are the instance's image
/// defaults (#985, [`AdoImageDefaults`]).
pub(crate) struct AdoBoardsToolkitConfig {
    inner: AdoToolkitConfig,
    images: AdoImageDefaults,
}

impl AdoBoardsToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AdoConfigError> {
        Ok(Self {
            inner: AdoToolkitConfig::parse(settings)?,
            images: AdoImageDefaults::parse(settings)?,
        })
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        self.inner.selected_tools()
    }

    pub(crate) fn into_inner(self) -> AdoToolkitConfig {
        self.inner
    }

    /// The connection settings and the instance's image defaults.
    pub(crate) fn into_parts(self) -> (AdoToolkitConfig, AdoImageDefaults) {
        (self.inner, self.images)
    }
}
