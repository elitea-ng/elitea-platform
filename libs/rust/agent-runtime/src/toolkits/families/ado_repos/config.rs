use serde_json::{Map, Value};

use crate::toolkits::families::ado::config::{AdoConfigError, AdoConnection, AdoToolkitConfig};

const DEFAULT_BRANCH: &str = "main";

/// `ado_repos` settings (`tools/ado/repos/__init__.py::get_toolkit`):
/// `ado_configuration`, `project`, the required `repository_id` (id or name),
/// and `base_branch`/`active_branch`, each `main` when absent or empty as in
/// `ReposApiWrapper.validate_toolkit`. The SDK also checks the repository and
/// both branches over the network while it validates; materialization here
/// stays network-free and the first call surfaces a wrong repository.
pub(crate) struct AdoReposToolkitConfig {
    inner: AdoToolkitConfig,
}

/// The repository settings left once the connection moved into a client.
pub(crate) struct AdoRepository {
    pub(crate) repository_id: Box<str>,
    pub(crate) base_branch: Box<str>,
    pub(crate) active_branch: Box<str>,
}

impl AdoReposToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, AdoConfigError> {
        let inner = AdoToolkitConfig::parse(settings)?;
        if inner.repository_id().is_none() {
            return Err(AdoConfigError::invalid());
        }
        Ok(Self { inner })
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        self.inner.selected_tools()
    }

    pub(crate) fn into_parts(self) -> (AdoConnection, AdoRepository) {
        let (connection, settings) = self.inner.into_parts();
        (
            connection,
            AdoRepository {
                repository_id: settings.repository_id.unwrap_or_default(),
                base_branch: settings
                    .base_branch
                    .unwrap_or_else(|| DEFAULT_BRANCH.into()),
                active_branch: settings
                    .active_branch
                    .unwrap_or_else(|| DEFAULT_BRANCH.into()),
            },
        )
    }
}
