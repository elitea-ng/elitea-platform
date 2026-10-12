//! The Confluence toolkit configuration as the indexing connector reads it
//! (the same settings the SDK's `ConfluenceToolkit` and the agent-runtime
//! family read: `confluence_configuration.{base_url,token|username+api_key,
//! hosting}`, `cloud`, `space`, `verify_ssl`, `custom_headers`), and the
//! `index_data` arguments the connector honours.

use serde_json::{Map, Value};
use url::Url;

use crate::atlassian::{
    AtlassianConfigError, AtlassianConfigErrorCode, Common, MAX_IDENTITY_BYTES, optional_flag,
    optional_text, safe_identifier, text_list,
};

pub(crate) const PROVIDER: &str = "Confluence";
const MAX_SPACE_BYTES: usize = 255;
const MAX_CQL_BYTES: usize = 8 * 1_024;
const MAX_PAGE_IDS: usize = 10_000;

/// Invocation-scoped Confluence authority for one index run.
pub struct ConfluenceConnectorConfig {
    /// The SDK's normalised `base_url` (a Cloud `/wiki` suffix removed);
    /// page links are built from it.
    base_url: Url,
    /// The REST root: `base_url`, plus `/wiki` for an Atlassian-hosted URL
    /// that lacks it (`Confluence.__init__`).
    api_root: Url,
    common: Common,
    space: Option<String>,
}

impl ConfluenceConnectorConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `confluence_configuration`, an invalid base URL, no
    /// credential, or an unsupported setting (plain HTTP, `verify_ssl:
    /// false`, a reserved custom header).
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let configuration = settings
            .get("confluence_configuration")
            .and_then(Value::as_object)
            .ok_or_else(|| {
                AtlassianConfigError::new(PROVIDER, AtlassianConfigErrorCode::InvalidConfiguration)
            })?;
        let (mut base_url, common) = Common::parse(settings, configuration, PROVIDER)?;
        // `validate_toolkit`: a Cloud base URL loses its `/wiki` suffix.
        if common.cloud && base_url.path().ends_with("/wiki") {
            let path = base_url.path().trim_end_matches("/wiki").to_owned();
            base_url.set_path(&path);
        }
        // An Atlassian URL without `/wiki` gets it (the library tests the URL
        // text, so `jira.com` counts too). Decided from the host and the
        // path, never the serialized URL: `https://wikiteam.atlassian.net`
        // has `/wiki` in its text (`//wikiteam`) and no `/wiki` path.
        let mut api_root = base_url.clone();
        let hosted = base_url
            .host_str()
            .is_some_and(|host| host.contains("atlassian.net") || host.contains("jira.com"));
        let has_wiki = base_url
            .path_segments()
            .is_some_and(|mut segments| segments.any(|segment| segment == "wiki"));
        if hosted && !has_wiki {
            let path = format!("{}/wiki", api_root.path().trim_end_matches('/'));
            api_root.set_path(&path);
        }
        let space = optional_text(settings, "space", MAX_SPACE_BYTES, PROVIDER)?
            .map(str::trim)
            .filter(|space| !space.is_empty())
            .map(ToOwned::to_owned);
        Ok(Self {
            base_url,
            api_root,
            common,
            space,
        })
    }

    #[must_use]
    pub fn base_url(&self) -> &Url {
        &self.base_url
    }

    #[must_use]
    pub fn api_root(&self) -> &Url {
        &self.api_root
    }

    #[must_use]
    pub fn common(&self) -> &Common {
        &self.common
    }

    #[must_use]
    pub fn cloud(&self) -> bool {
        self.common.cloud
    }

    /// The toolkit's space key, unquoted (`__sanitize_confluence_space`).
    #[must_use]
    pub fn space(&self) -> Option<&str> {
        let space = self.space.as_deref()?;
        Some(
            [('\'', '\''), ('`', '`'), ('"', '"')]
                .iter()
                .find_map(|(open, close)| {
                    space
                        .strip_prefix(*open)
                        .and_then(|rest| rest.strip_suffix(*close))
                })
                .unwrap_or(space),
        )
    }
}

/// The body representation `index_data` asks for (`content_format`).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum ContentFormat {
    #[default]
    View,
    Storage,
    ExportView,
    Editor,
    AnonymousExportView,
}

impl ContentFormat {
    /// The `body.<key>` the API expands.
    #[must_use]
    pub const fn body_key(self) -> &'static str {
        match self {
            Self::View => "view",
            Self::Storage => "storage",
            Self::ExportView => "export_view",
            Self::Editor => "editor",
            Self::AnonymousExportView => "anonymous_export_view",
        }
    }

    fn parse(value: &str) -> Option<Self> {
        Some(match value.trim().to_ascii_lowercase().as_str() {
            "view" => Self::View,
            "storage" => Self::Storage,
            "export_view" => Self::ExportView,
            "editor" => Self::Editor,
            "anonymous" | "anonymous_export_view" => Self::AnonymousExportView,
            _ => return None,
        })
    }
}

/// The `index_data` arguments the connector honours (the SDK's
/// `_index_tool_params`). `limit` and `max_pages` are accepted and ignored:
/// the connector lists everything (ADR-0030 decision 3) under the source's
/// document cap. `keep_markdown_format`, `keep_newlines`, `ocr_languages`
/// and `bins_with_llm` are accepted and ignored too: HTML becomes markdown
/// in the worker's extraction step, and model-calling steps wait for I4.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
#[allow(
    clippy::struct_excessive_bools,
    reason = "the SDK's index_data form is these flags"
)]
pub struct ConfluenceIndexArguments {
    pub content_format: ContentFormat,
    pub page_ids: Vec<String>,
    pub label: Option<String>,
    pub cql: Option<String>,
    pub include_restricted_content: bool,
    pub include_archived_content: bool,
    pub include_attachments: bool,
    pub include_extensions: Vec<String>,
    pub skip_extensions: Vec<String>,
    pub include_comments: bool,
    pub include_labels: bool,
}

impl ConfluenceIndexArguments {
    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// An argument of the wrong type, an unknown `content_format`, a page id
    /// that is not an identifier, or a CQL over its bound.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let invalid =
            || AtlassianConfigError::new(PROVIDER, AtlassianConfigErrorCode::InvalidConfiguration);
        let content_format = match optional_text(arguments, "content_format", 64, PROVIDER)? {
            None => ContentFormat::View,
            Some(value) if value.trim().is_empty() => ContentFormat::View,
            Some(value) => ContentFormat::parse(value).ok_or_else(invalid)?,
        };
        let page_ids: Vec<String> = match arguments.get("page_ids") {
            None | Some(Value::Null) => Vec::new(),
            Some(Value::Array(ids)) => ids
                .iter()
                .map(|id| match id {
                    Value::String(text) => Ok(text.trim().to_owned()),
                    Value::Number(number) => Ok(number.to_string()),
                    _ => Err(invalid()),
                })
                .collect::<Result<_, _>>()?,
            Some(Value::String(text)) => text
                .split([',', ';'])
                .map(str::trim)
                .filter(|id| !id.is_empty())
                .map(ToOwned::to_owned)
                .collect(),
            Some(_) => return Err(invalid()),
        };
        if page_ids.len() > MAX_PAGE_IDS {
            return Err(AtlassianConfigError::new(
                PROVIDER,
                AtlassianConfigErrorCode::ResourceExhausted,
            ));
        }
        if page_ids.iter().any(|id| !safe_identifier(id)) {
            return Err(invalid());
        }
        let flag =
            |name: &str| optional_flag(arguments, name, PROVIDER).map(Option::unwrap_or_default);
        Ok(Self {
            content_format,
            page_ids,
            label: optional_text(arguments, "label", MAX_IDENTITY_BYTES, PROVIDER)?
                .map(str::trim)
                .filter(|label| !label.is_empty())
                .map(ToOwned::to_owned),
            cql: optional_text(arguments, "cql", MAX_CQL_BYTES, PROVIDER)?
                .map(str::trim)
                .filter(|cql| !cql.is_empty())
                .map(ToOwned::to_owned),
            include_restricted_content: flag("include_restricted_content")?,
            include_archived_content: flag("include_archived_content")?,
            include_attachments: flag("include_attachments")?,
            include_extensions: text_list(arguments, "include_extensions", PROVIDER)?,
            skip_extensions: text_list(arguments, "skip_extensions", PROVIDER)?,
            include_comments: flag("include_comments")?,
            include_labels: flag("include_labels")?,
        })
    }

    /// Whether the run selects pages by anything but the toolkit's space
    /// (the SDK then ignores the space: `self.space_key = None`).
    #[must_use]
    pub fn selects_pages(&self) -> bool {
        self.cql.is_some() || self.label.is_some() || !self.page_ids.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn settings(space: Option<&str>, url: &str) -> Map<String, Value> {
        let mut settings = json!({"confluence_configuration": {"base_url": url, "token": "t"}})
            .as_object()
            .cloned()
            .unwrap();
        if let Some(space) = space {
            settings.insert("space".into(), Value::String(space.into()));
        }
        settings
    }

    #[test]
    fn cloud_urls_get_their_wiki_root_and_spaces_are_unquoted() {
        let config = ConfluenceConnectorConfig::parse(&settings(
            Some("'DOCS'"),
            "https://acme.atlassian.net/wiki",
        ))
        .expect("config");
        assert_eq!(config.base_url().as_str(), "https://acme.atlassian.net/");
        assert_eq!(
            config.api_root().as_str(),
            "https://acme.atlassian.net/wiki"
        );
        assert!(config.cloud());
        assert_eq!(config.space(), Some("DOCS"));

        // Review round 2, finding 5: `/wiki` is decided from the path. A
        // site whose NAME starts with `wiki` still gets the `/wiki` root.
        let named = ConfluenceConnectorConfig::parse(&settings(
            Some("DOCS"),
            "https://wikiteam.atlassian.net",
        ))
        .expect("config");
        assert_eq!(
            named.api_root().as_str(),
            "https://wikiteam.atlassian.net/wiki"
        );
        let named = ConfluenceConnectorConfig::parse(&settings(
            Some("DOCS"),
            "https://wikiteam.atlassian.net/wiki",
        ))
        .expect("config");
        assert_eq!(
            named.api_root().as_str(),
            "https://wikiteam.atlassian.net/wiki"
        );

        let server = ConfluenceConnectorConfig::parse(&settings(
            None,
            "https://wiki.corp.example/confluence/",
        ))
        .expect("server");
        assert_eq!(
            server.api_root().as_str(),
            "https://wiki.corp.example/confluence"
        );
        assert!(!server.cloud());
        assert_eq!(server.space(), None);

        assert!(
            ConfluenceConnectorConfig::parse(&json!({}).as_object().cloned().unwrap()).is_err()
        );
    }

    #[test]
    fn index_arguments_follow_the_sdk_form() {
        let arguments = json!({
            "content_format": "storage", "page_ids": ["123", 456], "label": "docs",
            "cql": "type=page", "include_attachments": true, "include_comments": true,
            "include_labels": true, "include_extensions": ["*.pdf"], "skip_extensions": "*.png;*.jpg",
            "limit": 10, "max_pages": 1000, "keep_markdown_format": true, "bins_with_llm": false,
        });
        let parsed = ConfluenceIndexArguments::parse(arguments.as_object().unwrap()).expect("ok");
        assert_eq!(parsed.content_format, ContentFormat::Storage);
        assert_eq!(parsed.page_ids, vec!["123", "456"]);
        assert_eq!(parsed.label.as_deref(), Some("docs"));
        assert_eq!(parsed.cql.as_deref(), Some("type=page"));
        assert!(parsed.include_attachments && parsed.include_comments && parsed.include_labels);
        assert!(!parsed.include_restricted_content);
        assert_eq!(parsed.skip_extensions, vec!["*.png", "*.jpg"]);
        assert!(parsed.selects_pages());
        let minimal = ConfluenceIndexArguments::parse(&Map::new()).expect("defaults");
        assert_eq!(minimal, ConfluenceIndexArguments::default());
        assert!(!minimal.selects_pages());
        for bad in [
            json!({"content_format": "pdf"}),
            json!({"page_ids": ["../x"]}),
            json!({"include_attachments": "yes"}),
            json!({"page_ids": 12}),
        ] {
            assert!(
                ConfluenceIndexArguments::parse(bad.as_object().unwrap()).is_err(),
                "{bad}"
            );
        }
    }
}
