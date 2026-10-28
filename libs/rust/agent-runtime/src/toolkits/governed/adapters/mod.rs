//! The reference adapters: Jira, Confluence, Aha!, `SharePoint` and qTest
//! (#1120), and remote MCP servers (#1123, [`mcp`]). Each family adapter maps
//! the tools its family serves today onto the operation catalogue
//! (`testdata/proto/runtime/v1/governed-operations/catalogue.json`) and
//! rebuilds its family from the frozen settings with the envelope's material
//! in place of any credential the settings carry.
//!
//! A tool a table does not list is not mapped, and a governed run cannot call
//! it: the generic request tools (`execute_generic_rq`,
//! `execute_generic_confluence`), every tool on a resource kind the catalogue
//! has no operation for (Aha! releases, ideas and generic records, qTest test
//! runs and defects, `SharePoint` list writes and `OneNote`), and any tool that
//! reaches past the configured target (`read_file_from_sharing_link`).

use std::sync::Arc;

use serde_json::{Map, Value};

use super::{AuthAdapterRegistry, AuthAdapterRegistryBuilder, GovernedError, canonical_target};
use crate::toolkits::families::https_base_url::{self, BasePath};

mod aha;
mod atlassian;
pub mod mcp;
mod qtest;
mod sharepoint;

pub use aha::AhaAdapter;
pub use atlassian::{ConfluenceAdapter, JiraAdapter};
pub use mcp::McpAdapter;
pub use qtest::QtestAdapter;
pub use sharepoint::SharePointAdapter;

/// The value a credential setting the family insists on, but a governed run
/// never uses, is replaced with (the `SharePoint` client secret).
pub(crate) const GOVERNED_UNUSED_SECRET: &str = "elitea-governed-unused";

/// A builder holding the six reference adapters, for a composition root to
/// extend with its own before [`AuthAdapterRegistryBuilder::build`].
#[must_use]
pub fn reference_registry_builder() -> AuthAdapterRegistryBuilder {
    let adapters: [Arc<dyn super::AuthAdapter>; 6] = [
        Arc::new(JiraAdapter),
        Arc::new(ConfluenceAdapter),
        Arc::new(AhaAdapter),
        Arc::new(SharePointAdapter),
        Arc::new(QtestAdapter),
        Arc::new(McpAdapter),
    ];
    // Six distinct provider types: no registration can collide.
    AuthAdapterRegistryBuilder {
        adapters: adapters
            .into_iter()
            .map(|adapter| (adapter.provider_type(), adapter))
            .collect(),
    }
}

/// The six reference adapters, built.
#[must_use]
pub fn reference_registry() -> AuthAdapterRegistry {
    reference_registry_builder().build()
}

/// The canonical `base_url` target of `<configuration>.base_url`.
pub(crate) fn base_url_target(
    settings: &Map<String, Value>,
    configuration: &str,
    path: BasePath,
) -> Result<String, GovernedError> {
    let raw = settings
        .get(configuration)
        .and_then(|value| value.get("base_url"))
        .and_then(Value::as_str)
        .ok_or(GovernedError::InvalidConfiguration)?;
    let url =
        https_base_url::parse(raw.trim(), path).map_err(|_| GovernedError::InvalidConfiguration)?;
    Ok(canonical_target(&url))
}

/// `settings` with `<configuration>` stripped of `strip` and given `insert`.
pub(crate) fn with_credential(
    settings: &Map<String, Value>,
    configuration: &str,
    strip: &[&str],
    insert: &[(&str, &str)],
) -> Result<Map<String, Value>, GovernedError> {
    let mut settings = settings.clone();
    let object = settings
        .get_mut(configuration)
        .and_then(Value::as_object_mut)
        .ok_or(GovernedError::InvalidConfiguration)?;
    for key in strip {
        object.remove(*key);
    }
    for (key, value) in insert {
        object.insert((*key).to_owned(), Value::String((*value).to_owned()));
    }
    Ok(settings)
}
