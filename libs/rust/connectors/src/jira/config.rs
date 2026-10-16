//! The Jira toolkit configuration as the indexing connector reads it (the
//! settings the SDK's `JiraToolkit` and the agent-runtime family read:
//! `jira_configuration.{base_url,token|username+api_key,hosting}`, `cloud`,
//! `api_version`, `verify_ssl`, `custom_headers`), and the `index_data`
//! arguments the connector honours.

use serde_json::{Map, Value};
use url::Url;

use crate::atlassian::{
    AtlassianConfigError, AtlassianConfigErrorCode, Common, optional_flag, optional_text, text_list,
};

pub(crate) const PROVIDER: &str = "Jira";
const MAX_JQL_BYTES: usize = 8 * 1_024;
const MAX_FIELDS: usize = 256;
const MAX_FIELD_BYTES: usize = 256;

/// The Jira REST API version every request uses (`/rest/api/{2,3}`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum JiraApiVersion {
    V2,
    V3,
}

impl JiraApiVersion {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::V2 => "2",
            Self::V3 => "3",
        }
    }
}

/// Invocation-scoped Jira authority for one index run.
pub struct JiraConnectorConfig {
    base_url: Url,
    common: Common,
    api_version: JiraApiVersion,
}

impl JiraConnectorConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `jira_configuration`, an invalid base URL, no credential,
    /// or an unsupported setting (plain HTTP, `verify_ssl: false`, a
    /// reserved custom header).
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let invalid =
            || AtlassianConfigError::new(PROVIDER, AtlassianConfigErrorCode::InvalidConfiguration);
        let configuration = settings
            .get("jira_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid)?;
        let (base_url, common) = Common::parse(settings, configuration, PROVIDER)?;
        // `_resolve_api_version`: an explicit 2/3 wins; anything else (Auto,
        // null) resolves from cloud, then from the host.
        let api_version = match settings.get("api_version") {
            Some(Value::String(value)) if value.trim() == "2" => JiraApiVersion::V2,
            Some(Value::String(value)) if value.trim() == "3" => JiraApiVersion::V3,
            Some(Value::Number(value)) if value.as_u64() == Some(2) => JiraApiVersion::V2,
            Some(Value::Number(value)) if value.as_u64() == Some(3) => JiraApiVersion::V3,
            None | Some(Value::Null | Value::String(_)) => {
                if common.cloud || common.host_is_cloud {
                    JiraApiVersion::V3
                } else {
                    JiraApiVersion::V2
                }
            }
            Some(_) => return Err(invalid()),
        };
        Ok(Self {
            base_url,
            common,
            api_version,
        })
    }

    #[must_use]
    pub fn base_url(&self) -> &Url {
        &self.base_url
    }

    #[must_use]
    pub fn common(&self) -> &Common {
        &self.common
    }

    #[must_use]
    pub fn cloud(&self) -> bool {
        self.common.cloud
    }

    #[must_use]
    pub fn api_version(&self) -> JiraApiVersion {
        self.api_version
    }
}

/// The `index_data` arguments the connector honours (the SDK's
/// `_index_tool_params`). `max_total_issues` is accepted and ignored: the
/// connector lists every issue the JQL matches (ADR-0030 decision 3) under
/// the source's document cap. `process_images` asks the worker to describe
/// the pictures an issue's description and comments link (ADR-0030 I4,
/// PR-C2), and `chunking_tool` is the worker's.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
#[allow(
    clippy::struct_excessive_bools,
    reason = "the SDK's index_data form is these flags"
)]
pub struct JiraIndexArguments {
    /// The JQL; `None` is the SDK's default of every issue, newest update
    /// first.
    pub jql: Option<String>,
    /// Fields requested beyond the defaults.
    pub fields_to_extract: Vec<String>,
    /// Fields rendered into the document after the description.
    pub fields_to_index: Vec<String>,
    pub include_attachments: bool,
    pub include_comments: bool,
    /// Lower-case dotted extensions (`.png`) of attachments left out.
    pub skip_attachment_extensions: Vec<String>,
    /// Whether a comment restricted to a group is listed, recorded as
    /// `Acl::Restricted` with that group (the Confluence argument of the
    /// same name). Without it every restricted comment is left out. A
    /// role-restricted comment and an issue with a security level are left
    /// out either way: neither names principals this runtime can record.
    pub include_restricted_content: bool,
    /// Describe the pictures the description and the comments link with the
    /// chat model (the SDK's `process_images`, off by default).
    pub process_images: bool,
}

impl JiraIndexArguments {
    /// The JQL the SDK sends when `jql` is not given.
    pub const DEFAULT_JQL: &'static str = "created >= \"1970-01-01\" ORDER BY updated DESC";

    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// An argument of the wrong type, or a list over its bound.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let fields = |name: &str| -> Result<Vec<String>, AtlassianConfigError> {
            let fields = text_list(arguments, name, PROVIDER)?;
            if fields.len() > MAX_FIELDS || fields.iter().any(|field| field.len() > MAX_FIELD_BYTES)
            {
                return Err(AtlassianConfigError::new(
                    PROVIDER,
                    AtlassianConfigErrorCode::ResourceExhausted,
                ));
            }
            Ok(fields)
        };
        let flag =
            |name: &str| optional_flag(arguments, name, PROVIDER).map(Option::unwrap_or_default);
        Ok(Self {
            jql: optional_text(arguments, "jql", MAX_JQL_BYTES, PROVIDER)?
                .map(str::trim)
                .filter(|jql| !jql.is_empty())
                .map(ToOwned::to_owned),
            fields_to_extract: fields("fields_to_extract")?,
            fields_to_index: fields("fields_to_index")?,
            include_attachments: flag("include_attachments")?,
            include_comments: flag("include_comments")?,
            include_restricted_content: flag("include_restricted_content")?,
            process_images: flag("process_images")?,
            skip_attachment_extensions: text_list(
                arguments,
                "skip_attachment_extensions",
                PROVIDER,
            )?
            .into_iter()
            .map(|extension| {
                let lower = extension.to_ascii_lowercase();
                if lower.starts_with('.') {
                    lower
                } else {
                    format!(".{lower}")
                }
            })
            .collect(),
        })
    }

    /// The JQL the listing sends.
    #[must_use]
    pub fn jql(&self) -> &str {
        self.jql.as_deref().unwrap_or(Self::DEFAULT_JQL)
    }

    /// Whether an attachment file name is left out by
    /// `skip_attachment_extensions` (the SDK compares the lower-case dotted
    /// extension exactly).
    #[must_use]
    pub fn skips_attachment(&self, filename: &str) -> bool {
        let extension = filename
            .rsplit_once('.')
            .map(|(_, extension)| format!(".{}", extension.to_ascii_lowercase()));
        extension.is_some_and(|extension| self.skip_attachment_extensions.contains(&extension))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn the_api_version_follows_the_sdk_resolution() {
        let parse = |settings: Value| JiraConnectorConfig::parse(settings.as_object().unwrap());
        let cloud = parse(
            json!({"jira_configuration": {"base_url": "https://acme.atlassian.net", "token": "t"}}),
        )
        .expect("cloud");
        assert_eq!(cloud.api_version(), JiraApiVersion::V3);
        assert!(cloud.cloud());
        let server = parse(json!({"jira_configuration": {"base_url": "https://jira.corp.example/jira", "token": "t"}}))
            .expect("server");
        assert_eq!(server.api_version(), JiraApiVersion::V2);
        assert_eq!(server.base_url().as_str(), "https://jira.corp.example/jira");
        let explicit = parse(json!({"api_version": "2", "jira_configuration": {"base_url": "https://acme.atlassian.net", "token": "t"}}))
            .expect("explicit");
        assert_eq!(explicit.api_version(), JiraApiVersion::V2);
        let auto = parse(json!({"api_version": "Auto", "cloud": true, "jira_configuration": {"base_url": "https://jira.corp.example", "token": "t"}}))
            .expect("auto");
        assert_eq!(auto.api_version(), JiraApiVersion::V3);
        assert!(parse(json!({})).is_err());
        assert!(parse(json!({"api_version": 7, "jira_configuration": {"base_url": "https://x.example", "token": "t"}})).is_err());
    }

    #[test]
    fn index_arguments_follow_the_sdk_form() {
        let parsed = JiraIndexArguments::parse(
            json!({
                "jql": " project = X ", "fields_to_extract": ["labels", "Priority"], "fields_to_index": "labels,priority",
                "include_attachments": true, "include_comments": false, "skip_attachment_extensions": ["PNG", ".jpg"],
                "max_total_issues": 10, "process_images": true, "chunking_tool": "markdown",
            })
            .as_object()
            .unwrap(),
        )
        .expect("ok");
        assert_eq!(parsed.jql(), "project = X");
        assert_eq!(parsed.fields_to_extract, vec!["labels", "Priority"]);
        assert_eq!(parsed.fields_to_index, vec!["labels", "priority"]);
        assert!(parsed.include_attachments && !parsed.include_comments);
        assert!(!parsed.include_restricted_content);
        assert_eq!(parsed.skip_attachment_extensions, vec![".png", ".jpg"]);
        assert!(parsed.skips_attachment("shot.PNG"));
        assert!(!parsed.skips_attachment("spec.pdf"));
        assert!(!parsed.skips_attachment("README"));
        let minimal = JiraIndexArguments::parse(&Map::new()).expect("defaults");
        assert_eq!(minimal.jql(), JiraIndexArguments::DEFAULT_JQL);
        assert!(
            JiraIndexArguments::parse(json!({"include_comments": "no"}).as_object().unwrap())
                .is_err()
        );
    }
}
