use serde_json::{Map, Value};

use crate::toolkits::families::ado::config::{AdoConfigError, AdoToolkitConfig};

/// `ado_boards` settings (`tools/ado/work_item/__init__.py::get_toolkit`):
/// `ado_configuration`, `project`, `limit` (default 5) and `selected_tools`.
/// The pgvector and embedding settings only feed the index tools, which this
/// runtime does not serve.
pub(crate) struct AdoBoardsToolkitConfig {
    inner: AdoToolkitConfig,
}

impl AdoBoardsToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AdoConfigError> {
        Ok(Self {
            inner: AdoToolkitConfig::parse(settings)?,
        })
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        self.inner.selected_tools()
    }

    pub(crate) fn into_inner(self) -> AdoToolkitConfig {
        self.inner
    }
}
