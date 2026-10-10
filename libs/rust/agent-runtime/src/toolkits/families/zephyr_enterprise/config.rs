use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use crate::toolkits::families::zephyr_rest::config::{
    ZephyrRestConfigError, bearer_token, configuration_object, required_https_base, selected_tools,
};

/// Invocation-scoped Zephyr Enterprise authority from one accepted claim.
///
/// Main freezes the `zephyr_enterprise` configuration under
/// `zephyr_configuration` with the redeemed token. The SDK schema marks the
/// token optional, but its client refuses to start without one ("You have to
/// declare token ..."), so it is required here too. Non-`Clone`, non-`Debug`.
pub(crate) struct ZephyrEnterpriseToolkitConfig {
    base_url: Url,
    token: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl ZephyrEnterpriseToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ZephyrRestConfigError> {
        let configuration = configuration_object(settings, "zephyr_configuration")?;
        Ok(Self {
            base_url: required_https_base(configuration, "base_url")?,
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
