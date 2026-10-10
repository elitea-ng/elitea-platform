use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use crate::toolkits::families::zephyr_rest::config::{
    ZephyrRestConfigError, bearer_token, configuration_object, optional_https_base, selected_tools,
};

/// The SDK wrapper's default when the configuration has no base URL.
pub(in crate::toolkits) const DEFAULT_BASE_URL: &str = "https://prod-api.zephyr4jiracloud.com/v2";

/// Invocation-scoped Zephyr Essential authority from one accepted claim.
///
/// Main freezes `zephyr_essential_configuration` (`base_url`, redeemed
/// `token`). Non-`Clone`, non-`Debug`; never read from the environment.
pub(crate) struct ZephyrEssentialToolkitConfig {
    base_url: Url,
    token: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl ZephyrEssentialToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ZephyrRestConfigError> {
        let configuration = configuration_object(settings, "zephyr_essential_configuration")?;
        Ok(Self {
            base_url: optional_https_base(configuration, "base_url", DEFAULT_BASE_URL)?,
            token: bearer_token(configuration, "token")?,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) const fn base_url(&self) -> &Url {
        &self.base_url
    }

    pub(super) fn into_parts(self) -> (Url, Zeroizing<String>, Vec<Box<str>>) {
        (self.base_url, self.token, self.selected_tools)
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}
