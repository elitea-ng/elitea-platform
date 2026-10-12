use serde_json::{Map, Value};

use crate::toolkits::families::ado::config::{AdoConfigError, AdoToolkitConfig};

/// `ado_wiki` settings (`tools/ado/__init__.py::get_tools`):
/// `ado_configuration`, `project`, `limit`, `selected_tools` and the optional
/// `default_wiki_identifier`.
/// The pgvector and embedding settings only feed the index tools, which this
/// runtime does not serve.
pub(crate) struct AdoWikiToolkitConfig {
    inner: AdoToolkitConfig,
}

impl AdoWikiToolkitConfig {
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
