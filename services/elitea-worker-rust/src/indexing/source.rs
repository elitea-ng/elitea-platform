//! The toolkit type to its `ContentSource` connector, and the `index_data`
//! path filters.
//!
//! I1 serves the five families whose connectors exist (ADR-0030 decision 7):
//! `github`, `gitlab`, `bitbucket`, `ado_repos` and `artifact`; I2 adds the
//! Atlassian pair, `confluence` and `jira` (pages or issues with their
//! attachments and comments as dependent documents). Every other indexing
//! toolkit type is refused with its name, so a run on a deployment that
//! switched too early fails with a reason instead of indexing nothing.
//!
//! The whitelist and blacklist keep the SDK's rule (`CodeIndexerToolkit`):
//! a path is kept when it matches any whitelist pattern (or the whitelist is
//! empty) and matches no blacklist pattern, where a pattern matches by Unix
//! shell wildcards (`fnmatch`) or as a bare extension (`py` matches `a.py`).

use std::sync::Arc;

use elitea_connectors::ado::repos::AdoReposToolkitConfig;
use elitea_connectors::ado::source::AdoReposSource;
use elitea_connectors::artifact::client::ArtifactPlatform;
use elitea_connectors::artifact::source::ArtifactSource;
use elitea_connectors::bitbucket::config::BitbucketToolkitConfig;
use elitea_connectors::bitbucket::source::BitbucketSource;
use elitea_connectors::confluence::config::{ConfluenceConnectorConfig, ConfluenceIndexArguments};
use elitea_connectors::confluence::source::ConfluenceSource;
use elitea_connectors::egress::HostAllowlist;
use elitea_connectors::github::config::GitHubToolkitConfig;
use elitea_connectors::github::source::GitHubSource;
use elitea_connectors::gitlab::config::GitLabToolkitConfig;
use elitea_connectors::gitlab::source::GitLabSource;
use elitea_connectors::jira::config::{JiraConnectorConfig, JiraIndexArguments};
use elitea_connectors::jira::source::JiraSource;
use elitea_connectors::source::SourceLimits;
use elitea_connectors::transport::Transport;
use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::Value;

use super::IndexRunLimits;
use super::inputs::{IndexParameters, ToolkitConfiguration};

/// The families I1 indexes.
pub(crate) const SERVED_TOOLKIT_TYPES: [&str; 5] =
    ["github", "gitlab", "bitbucket", "ado_repos", "artifact"];
/// The I2 families this worker indexes so far (ADR-0030 decision 7; the
/// Microsoft four land apart).
pub(crate) const I2_SERVED_TOOLKIT_TYPES: [&str; 2] = ["confluence", "jira"];

/// A connector could not be built. The text names the toolkit type and the
/// connector's own data-free reason.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct SourceBuildError(pub(crate) String);

impl std::fmt::Display for SourceBuildError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl std::error::Error for SourceBuildError {}

/// One toolkit's documents.
pub(crate) enum IndexSource {
    GitHub(GitHubSource),
    GitLab(GitLabSource),
    Bitbucket(BitbucketSource),
    AdoRepos(AdoReposSource),
    Artifact(ArtifactSource),
    Confluence(ConfluenceSource),
    Jira(JiraSource),
    #[cfg(test)]
    Fixture(Vec<Document>),
}

/// What the artifact connector needs of the platform: the object API's
/// base, the claim's bearer and the project.
pub(crate) struct PlatformAccess<'a> {
    pub(crate) origin: &'a str,
    pub(crate) bearer: &'a str,
    pub(crate) project_id: &'a str,
}

impl IndexSource {
    /// The connector for `configuration`'s toolkit, at the branch
    /// `parameters` names (the code families), through `allowlist` and
    /// `transport`, bounded by `limits`.
    ///
    /// # Errors
    ///
    /// An unserved toolkit type, settings the connector refuses, or a host
    /// the allowlist does not permit.
    pub(crate) fn build(
        configuration: &ToolkitConfiguration,
        parameters: &IndexParameters,
        platform: &PlatformAccess<'_>,
        allowlist: &HostAllowlist,
        transport: Arc<dyn Transport>,
        limits: IndexRunLimits,
    ) -> Result<Self, SourceBuildError> {
        let kind = configuration.toolkit_type.as_str();
        let refused = |reason: &dyn std::fmt::Display| {
            SourceBuildError(format!("the {kind} toolkit cannot be indexed: {reason}"))
        };
        let source_limits = SourceLimits {
            max_documents: limits.max_documents,
            max_document_bytes: limits.max_document_bytes,
            ..SourceLimits::default()
        };
        let settings = &configuration.settings;
        let source = match kind {
            "github" => {
                let config = GitHubToolkitConfig::parse(settings).map_err(|e| refused(&e))?;
                let mut source = GitHubSource::new(config, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                if let Some(branch) = &parameters.branch {
                    source = source.with_branch(branch);
                }
                Self::GitHub(source.with_limits(source_limits))
            }
            "gitlab" => {
                let config = GitLabToolkitConfig::parse(settings).map_err(|e| refused(&e))?;
                let mut source = GitLabSource::new(config, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                if let Some(branch) = &parameters.branch {
                    source = source.with_branch(branch);
                }
                Self::GitLab(source.with_limits(source_limits))
            }
            "bitbucket" => {
                let config = BitbucketToolkitConfig::parse(settings).map_err(|e| refused(&e))?;
                let mut source = BitbucketSource::new(config, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                if let Some(branch) = &parameters.branch {
                    source = source.with_branch(branch);
                }
                Self::Bitbucket(source.with_limits(source_limits))
            }
            "ado_repos" => {
                let config = AdoReposToolkitConfig::parse(settings).map_err(|e| refused(&e))?;
                let mut source = AdoReposSource::new(config, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                if let Some(branch) = &parameters.branch {
                    source = source.with_branch(branch);
                }
                Self::AdoRepos(source.with_limits(source_limits))
            }
            "artifact" => {
                let bucket = match settings.get("bucket") {
                    Some(Value::String(bucket)) if !bucket.trim().is_empty() => bucket.trim(),
                    _ => return Err(refused(&"the settings name no `bucket`")),
                };
                let api =
                    ArtifactPlatform::new(platform.origin, platform.bearer, platform.project_id)
                        .map_err(|e| refused(&e))?;
                let source = ArtifactSource::new(api, bucket, "", allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::Artifact(source.with_limits(source_limits))
            }
            "confluence" => {
                let config = ConfluenceConnectorConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments = ConfluenceIndexArguments::parse(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source = ConfluenceSource::new(config, arguments, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::Confluence(source.with_limits(source_limits))
            }
            "jira" => {
                let config = JiraConnectorConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments =
                    JiraIndexArguments::parse(&parameters.arguments).map_err(|e| refused(&e))?;
                let source = JiraSource::new(config, arguments, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::Jira(source.with_limits(source_limits))
            }
            // The tests' in-memory toolkit: the documents the test staged.
            #[cfg(test)]
            "fixture" => match super::tests::take_fixture_source() {
                Some(source) => source,
                None => return Err(SourceBuildError("no fixture documents staged".to_owned())),
            },
            other => {
                return Err(SourceBuildError(format!(
                    "the {other} toolkit is not indexed by this worker yet (ADR-0030 I1 serves {}; I2 so far {})",
                    SERVED_TOOLKIT_TYPES.join(", "),
                    I2_SERVED_TOOLKIT_TYPES.join(", ")
                )));
            }
        };
        Ok(source)
    }

    /// Every document the source holds.
    ///
    /// # Errors
    ///
    /// The connector's listing failure.
    pub(crate) async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        match self {
            Self::GitHub(source) => source.list().await,
            Self::GitLab(source) => source.list().await,
            Self::Bitbucket(source) => source.list().await,
            Self::AdoRepos(source) => source.list().await,
            Self::Artifact(source) => source.list().await,
            Self::Confluence(source) => source.list().await,
            Self::Jira(source) => source.list().await,
            #[cfg(test)]
            Self::Fixture(documents) => Ok(documents
                .iter()
                .map(|document| document.reference.clone())
                .collect()),
        }
    }

    /// One document's content.
    ///
    /// # Errors
    ///
    /// The connector's fetch failure.
    pub(crate) async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        match self {
            Self::GitHub(source) => source.fetch(key).await,
            Self::GitLab(source) => source.fetch(key).await,
            Self::Bitbucket(source) => source.fetch(key).await,
            Self::AdoRepos(source) => source.fetch(key).await,
            Self::Artifact(source) => source.fetch(key).await,
            Self::Confluence(source) => source.fetch(key).await,
            Self::Jira(source) => source.fetch(key).await,
            #[cfg(test)]
            Self::Fixture(documents) => documents
                .iter()
                .find(|document| document.reference.key == key)
                .cloned()
                .ok_or_else(|| SourceError::NotFound(key.to_owned()))
                .and_then(|document| {
                    // A listed document whose fetch fails (the test marks it
                    // with this metadata key).
                    if document.metadata.contains_key("fixture_unfetchable") {
                        Err(SourceError::Unavailable(format!(
                            "fixture: '{key}' cannot be fetched"
                        )))
                    } else {
                        Ok(document)
                    }
                }),
        }
    }
}

/// The `index_data` whitelist and blacklist.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(crate) struct PathFilter {
    whitelist: Vec<String>,
    blacklist: Vec<String>,
}

impl PathFilter {
    #[must_use]
    pub(crate) fn new(whitelist: &[String], blacklist: &[String]) -> Self {
        Self {
            whitelist: whitelist.to_vec(),
            blacklist: blacklist.to_vec(),
        }
    }

    /// Whether a document path is indexed: on the whitelist (or no
    /// whitelist) and not on the blacklist, by the SDK's rule.
    #[must_use]
    pub(crate) fn keeps(&self, path: &str) -> bool {
        let listed = |patterns: &[String]| {
            patterns
                .iter()
                .any(|pattern| fnmatch(pattern, path) || path.ends_with(&format!(".{pattern}")))
        };
        (self.whitelist.is_empty() || listed(&self.whitelist)) && !listed(&self.blacklist)
    }
}

/// Python's `fnmatch.fnmatch`: `*` any run, `?` one character, `[seq]` one
/// of, `[!seq]` none of; `/` is not special. Case-sensitive, as on Linux.
#[must_use]
pub(crate) fn fnmatch(pattern: &str, text: &str) -> bool {
    let pattern: Vec<char> = pattern.chars().collect();
    let text: Vec<char> = text.chars().collect();
    matches_from(&pattern, &text)
}

fn matches_from(pattern: &[char], text: &[char]) -> bool {
    // Iterative with one backtrack point, the classic `*` algorithm.
    let (mut p, mut t) = (0, 0);
    let mut star: Option<(usize, usize)> = None;
    while t < text.len() {
        if p < pattern.len() {
            match pattern[p] {
                '*' => {
                    star = Some((p, t));
                    p += 1;
                    continue;
                }
                '?' => {
                    p += 1;
                    t += 1;
                    continue;
                }
                '[' => {
                    if let Some((matched, next)) = class(pattern, p, text[t]) {
                        if matched {
                            p = next;
                            t += 1;
                            continue;
                        }
                    } else if text[t] == '[' {
                        p += 1;
                        t += 1;
                        continue;
                    }
                }
                literal if literal == text[t] => {
                    p += 1;
                    t += 1;
                    continue;
                }
                _ => {}
            }
        }
        match star {
            Some((star_p, star_t)) => {
                p = star_p + 1;
                t = star_t + 1;
                star = Some((star_p, star_t + 1));
            }
            None => return false,
        }
    }
    pattern[p..].iter().all(|c| *c == '*')
}

/// A bracket class at `start`: whether `character` is in it, and the index
/// after `]`. `None` when the bracket is not closed (then it is literal).
fn class(pattern: &[char], start: usize, character: char) -> Option<(bool, usize)> {
    let mut index = start + 1;
    let negated = index < pattern.len() && matches!(pattern[index], '!' | '^');
    if negated {
        index += 1;
    }
    let mut matched = false;
    let mut first = true;
    while index < pattern.len() {
        let current = pattern[index];
        if current == ']' && !first {
            return Some((matched != negated, index + 1));
        }
        first = false;
        if index + 2 < pattern.len() && pattern[index + 1] == '-' && pattern[index + 2] != ']' {
            let (low, high) = (current, pattern[index + 2]);
            if low <= character && character <= high {
                matched = true;
            }
            index += 3;
        } else {
            if current == character {
                matched = true;
            }
            index += 1;
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fnmatch_follows_python() {
        assert!(fnmatch("*.py", "src/app.py"));
        assert!(fnmatch("*test_*", "tests/test_app.py"));
        assert!(fnmatch("src/?.rs", "src/a.rs"));
        assert!(!fnmatch("src/?.rs", "src/ab.rs"));
        assert!(fnmatch("[abc].md", "b.md"));
        assert!(!fnmatch("[!abc].md", "b.md"));
        assert!(fnmatch("[a-c].md", "c.md"));
        assert!(fnmatch("docs/**", "docs/a/b.md"));
        assert!(fnmatch("*", ""));
        assert!(!fnmatch("?", ""));
        assert!(fnmatch("a[", "a["));
        assert!(fnmatch("README", "README"));
        assert!(!fnmatch("readme", "README"));
    }

    #[test]
    fn the_filter_keeps_the_sdk_rule() {
        let filter = PathFilter::new(
            &["py".to_owned(), "*.md".to_owned()],
            &["*test_*".to_owned()],
        );
        assert!(filter.keeps("src/app.py"));
        assert!(filter.keeps("README.md"));
        assert!(!filter.keeps("src/app.rs"));
        assert!(!filter.keeps("tests/test_app.py"));
        assert!(PathFilter::default().keeps("anything"));
        let blacklist_only = PathFilter::new(&[], &["lock".to_owned()]);
        assert!(blacklist_only.keeps("Cargo.toml"));
        assert!(!blacklist_only.keeps("Cargo.lock"));
    }

    #[test]
    fn an_unserved_toolkit_type_is_refused_with_its_name() {
        let configuration = ToolkitConfiguration {
            toolkit_type: "figma".to_owned(),
            toolkit_name: "designs".to_owned(),
            settings: serde_json::Map::new(),
        };
        let parameters = IndexParameters {
            index_name: "x".to_owned(),
            clean_index: false,
            chunking_tool: None,
            chunking_config: Value::Null,
            branch: None,
            whitelist: Vec::new(),
            blacklist: Vec::new(),
            arguments: serde_json::Map::new(),
        };
        let platform = PlatformAccess {
            origin: "https://platform",
            bearer: "pat",
            project_id: "1",
        };
        let error = IndexSource::build(
            &configuration,
            &parameters,
            &platform,
            &HostAllowlist::parse(Some("*")),
            Arc::new(
                elitea_connectors::reqwest012::ReqwestTransport::hardened(
                    &elitea_connectors::github::client::CLIENT_POLICY,
                )
                .expect("client"),
            ),
            IndexRunLimits::default(),
        )
        .err()
        .expect("refused");
        assert!(error.0.contains("figma"), "{error}");
        assert!(error.0.contains("ADR-0030 I1"), "{error}");
        assert!(error.0.contains("confluence, jira"), "{error}");
    }

    /// The Atlassian families are built from the toolkit settings and the
    /// `index_data` arguments; a refused configuration or argument names the
    /// toolkit type and the connector's data-free reason.
    #[test]
    fn the_atlassian_families_are_built_or_refused_with_a_reason() {
        let platform = PlatformAccess {
            origin: "https://platform",
            bearer: "pat",
            project_id: "1",
        };
        let transport: Arc<dyn Transport> = Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("client"),
        );
        let build = |kind: &str, settings: Value, arguments: Value| {
            let configuration = ToolkitConfiguration {
                toolkit_type: kind.to_owned(),
                toolkit_name: "kit".to_owned(),
                settings: settings.as_object().cloned().unwrap_or_default(),
            };
            let parameters = IndexParameters {
                index_name: "x".to_owned(),
                clean_index: false,
                chunking_tool: None,
                chunking_config: Value::Null,
                branch: None,
                whitelist: Vec::new(),
                blacklist: Vec::new(),
                arguments: arguments.as_object().cloned().unwrap_or_default(),
            };
            IndexSource::build(
                &configuration,
                &parameters,
                &platform,
                &HostAllowlist::parse(Some("*")),
                Arc::clone(&transport),
                IndexRunLimits::default(),
            )
        };
        let confluence = serde_json::json!({
            "space": "DOCS",
            "confluence_configuration": {"base_url": "https://acme.atlassian.net/wiki", "token": "t"},
        });
        assert!(matches!(
            build(
                "confluence",
                confluence.clone(),
                serde_json::json!({"include_attachments": true})
            ),
            Ok(IndexSource::Confluence(_))
        ));
        // No space and nothing selecting pages: refused before any request.
        let error = build(
            "confluence",
            serde_json::json!({"confluence_configuration": {"base_url": "https://acme.atlassian.net", "token": "t"}}),
            serde_json::json!({}),
        )
        .err()
        .expect("refused");
        assert!(
            error
                .0
                .starts_with("the confluence toolkit cannot be indexed:"),
            "{error}"
        );
        assert!(error.0.contains("`space`"), "{error}");
        // A bad argument is refused with the connector's reason.
        let error = build(
            "confluence",
            confluence,
            serde_json::json!({"content_format": "pdf"}),
        )
        .err()
        .expect("refused");
        assert!(error.0.contains("configuration is invalid"), "{error}");

        let jira = serde_json::json!({
            "jira_configuration": {"base_url": "https://jira.corp.example", "username": "u", "api_key": "k"},
        });
        assert!(matches!(
            build(
                "jira",
                jira,
                serde_json::json!({"jql": "project = X", "include_comments": true})
            ),
            Ok(IndexSource::Jira(_))
        ));
        let error = build(
            "jira",
            serde_json::json!({"jira_configuration": {"base_url": "http://jira.corp.example", "token": "t"}}),
            serde_json::json!({}),
        )
        .err()
        .expect("plain http refused");
        assert!(error.0.contains("plain HTTP"), "{error}");
    }
}
