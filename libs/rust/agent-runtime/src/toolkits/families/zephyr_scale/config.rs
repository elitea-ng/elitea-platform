use reqwest::Url;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use crate::toolkits::families::zephyr_rest::config::{
    ZephyrRestConfigError, bearer_token, configuration_object, parse_https_base, selected_tools,
};

/// `zephyr-python-api`'s `DEFAULT_BASE_URL`, the only origin the SDK's
/// Zephyr Scale wrapper ever calls.
pub(in crate::toolkits) const CLOUD_API: &str = "https://api.zephyrscale.smartbear.com/v2";

/// Invocation-scoped Zephyr Scale authority from one accepted claim.
///
/// Main freezes the shared `zephyr` configuration under
/// `zephyr_configuration`. Its `base_url`, `username`, `password` and
/// `cookies` are accepted and ignored: the SDK constructs
/// `ZephyrScale(token=values['token'])`, so only the token is ever used, and
/// without it the SDK fails at construction. The token is required here.
pub(crate) struct ZephyrScaleToolkitConfig {
    base_url: Url,
    token: Zeroizing<String>,
    selected_tools: Vec<Box<str>>,
}

impl ZephyrScaleToolkitConfig {
    pub(crate) fn parse(settings: &Map<String, Value>) -> Result<Self, ZephyrRestConfigError> {
        let configuration = configuration_object(settings, "zephyr_configuration")?;
        Ok(Self {
            base_url: parse_https_base(CLOUD_API)?,
            token: bearer_token(configuration, "token")?,
            selected_tools: selected_tools(settings)?,
        })
    }

    pub(super) fn into_parts(self) -> (Url, Zeroizing<String>, Vec<Box<str>>) {
        (self.base_url, self.token, self.selected_tools)
    }

    #[must_use]
    pub(crate) fn selected_tools(&self) -> &[Box<str>] {
        &self.selected_tools
    }
}
