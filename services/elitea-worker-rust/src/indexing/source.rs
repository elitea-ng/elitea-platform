//! The toolkit type to its `ContentSource` connector, and the `index_data`
//! path filters.
//!
//! I1 serves the five families whose connectors exist (ADR-0030 decision 7):
//! `github`, `gitlab`, `bitbucket`, `ado_repos` and `artifact`; I2 adds the
//! Atlassian pair, `confluence` and `jira` (pages or issues with their
//! attachments and comments as dependent documents), and `sharepoint`,
//! `ado_wiki`, `ado_boards` and `ado_plans`; I3 adds `figma` (frames, and comments on
//! request). Every other
//! indexing toolkit type is refused with its name, so a run on a deployment
//! that switched too early fails with a reason instead of indexing nothing.
//!
//! The family-specific `index_data` arguments (`wiql`, `plan_id`,
//! `wiki_identifier`, `path`, …) reach the connector as the raw argument map
//! (`IndexParameters::arguments`); each connector reads its own strictly.
//!
//! The whitelist and blacklist keep the SDK's rule (`CodeIndexerToolkit`):
//! a path is kept when it matches any whitelist pattern (or the whitelist is
//! empty) and matches no blacklist pattern, where a pattern matches by Unix
//! shell wildcards (`fnmatch`, the connectors' `source::glob` clone of
//! Python's) or as a bare extension (`py` matches `a.py`).

use std::collections::BTreeMap;
use std::sync::Arc;

use elitea_connectors::ado::boards_source::{AdoBoardsIndexParams, AdoBoardsSource};
use elitea_connectors::ado::config::AdoToolkitConfig;
use elitea_connectors::ado::plans_source::{AdoPlansIndexParams, AdoPlansSource};
use elitea_connectors::ado::repos::AdoReposToolkitConfig;
use elitea_connectors::ado::source::AdoReposSource;
use elitea_connectors::ado::wiki_source::{AdoWikiIndexParams, AdoWikiSource};
use elitea_connectors::artifact::source::ArtifactSource;
use elitea_connectors::bitbucket::config::BitbucketToolkitConfig;
use elitea_connectors::bitbucket::source::BitbucketSource;
use elitea_connectors::confluence::config::{ConfluenceConnectorConfig, ConfluenceIndexArguments};
use elitea_connectors::confluence::source::ConfluenceSource;
use elitea_connectors::egress::HostAllowlist;
use elitea_connectors::figma::config::{FigmaConnectorConfig, FigmaIndexArguments};
use elitea_connectors::figma::source::FigmaSource;
use elitea_connectors::github::config::GitHubToolkitConfig;
use elitea_connectors::github::source::GitHubSource;
use elitea_connectors::gitlab::config::GitLabToolkitConfig;
use elitea_connectors::gitlab::source::GitLabSource;
use elitea_connectors::jira::config::{JiraConnectorConfig, JiraIndexArguments};
use elitea_connectors::jira::source::JiraSource;
use elitea_connectors::pictures::{PictureBatch, PictureStep};
use elitea_connectors::qtest::{QtestConnectorConfig, QtestIndexArguments, QtestSource};
use elitea_connectors::sharepoint::config::SharePointIndexConfig;
use elitea_connectors::sharepoint::source::{SharePointIndexParams, SharePointSource};
use elitea_connectors::source::SourceLimits;
use elitea_connectors::source::glob::fnmatch;
use elitea_connectors::testrail::{
    TestRailConnectorConfig, TestRailIndexArguments, TestRailSource,
};
use elitea_connectors::transport::Transport;
use elitea_connectors::xray::{XrayConnectorConfig, XrayIndexArguments, XraySource};
use elitea_connectors::zephyr::enterprise::{
    ZephyrEnterpriseConfig, ZephyrEnterpriseIndexArguments, ZephyrEnterpriseSource,
};
use elitea_connectors::zephyr::essential::{ZephyrEssentialConfig, ZephyrEssentialSource};
use elitea_connectors::zephyr::scale::{
    ZephyrScaleConfig, ZephyrScaleIndexArguments, ZephyrScaleSource,
};
use elitea_content_source::supported::has_supported_extension;
use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use elitea_platform_rpc::artifacts::ArtifactClient;
use serde_json::Value;

use super::IndexRunLimits;
use super::inputs::{IndexParameters, ToolkitConfiguration};
use crate::transport::artifact_objects::ProjectObjects;

/// The families this worker indexes (ADR-0030 I1, I2 and I3); the Go
/// `indexcatalog.Families` and agent-runtime `INDEX_FAMILIES` list the same
/// types (tested against `index_served_families.json`).
pub(crate) const SERVED_TOOLKIT_TYPES: [&str; 18] = [
    "github",
    "gitlab",
    "bitbucket",
    "ado_repos",
    "artifact",
    "confluence",
    "jira",
    "sharepoint",
    "ado_wiki",
    "ado_boards",
    "ado_plans",
    "figma",
    "testrail",
    "qtest",
    "xray_cloud",
    "zephyr_scale",
    "zephyr_essential",
    "zephyr_enterprise",
];

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
    /// The artifact connector and the folder it lists under (`""` for the
    /// bucket). The connector keys objects relative to the folder; this
    /// worker keys them by the FULL object key, as the SDK does, so a run
    /// over `a/` and one over `b/` cannot collide.
    Artifact(ArtifactSource, String),
    Confluence(ConfluenceSource),
    Jira(JiraSource),
    SharePoint(SharePointSource),
    AdoWiki(AdoWikiSource),
    AdoBoards(AdoBoardsSource),
    AdoPlans(AdoPlansSource),
    Figma(FigmaSource),
    TestRail(TestRailSource),
    Qtest(QtestSource),
    Xray(XraySource),
    ZephyrScale(ZephyrScaleSource),
    ZephyrEssential(ZephyrEssentialSource),
    ZephyrEnterprise(ZephyrEnterpriseSource),
    #[cfg(test)]
    Fixture(Vec<Document>, PathFilter),
}

/// What the artifact connector needs of the platform: its artifact store
/// (`ArtifactObjectService` on elitea-main's platform listener, never the
/// connectors' transport to third-party hosts), the claim's bearer and the
/// project.
pub(crate) struct PlatformAccess<'a> {
    pub(crate) artifacts: &'a ArtifactClient,
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
    #[allow(clippy::too_many_lines)] // One arm per served family.
    pub(crate) fn build(
        configuration: &ToolkitConfiguration,
        parameters: &IndexParameters,
        filter: &PathFilter,
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
                let objects = ProjectObjects::new(
                    platform.artifacts.clone(),
                    platform.bearer,
                    platform.project_id,
                )
                .ok_or_else(|| refused(&"the run has no usable credential or project"))?;
                // The SDK's `folder`: list under that prefix only. Parsed
                // once, with the filter.
                let folder = filter.artifact_folder();
                // The platform's own artifact store, never the connectors'
                // transport.
                let source = ArtifactSource::new(Arc::new(objects), bucket, folder)
                    .map_err(|e| refused(&e))?;
                Self::Artifact(source.with_limits(source_limits), folder.to_owned())
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
            "sharepoint" => {
                let config = SharePointIndexConfig::parse(settings).map_err(|e| refused(&e))?;
                let params = SharePointIndexParams::from_arguments(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source = SharePointSource::new(config, params, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::SharePoint(source.with_limits(source_limits))
            }
            "ado_wiki" => {
                let config = AdoToolkitConfig::parse(settings).map_err(|e| refused(&e))?;
                let params = AdoWikiIndexParams::from_arguments(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source = AdoWikiSource::new(config, params, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::AdoWiki(source.with_limits(source_limits))
            }
            "ado_boards" => {
                let config = AdoToolkitConfig::parse(settings).map_err(|e| refused(&e))?;
                let params = AdoBoardsIndexParams::from_arguments(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source = AdoBoardsSource::new(config, params, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::AdoBoards(source.with_limits(source_limits))
            }
            "ado_plans" => {
                let config = AdoToolkitConfig::parse(settings).map_err(|e| refused(&e))?;
                let params = AdoPlansIndexParams::from_arguments(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source = AdoPlansSource::new(config, params, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::AdoPlans(source.with_limits(source_limits))
            }
            "figma" => {
                let config = FigmaConnectorConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments =
                    FigmaIndexArguments::parse(&parameters.arguments).map_err(|e| refused(&e))?;
                let source = FigmaSource::new(config, arguments, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::Figma(source.with_limits(source_limits))
            }
            "testrail" => {
                let config = TestRailConnectorConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments = TestRailIndexArguments::parse(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source = TestRailSource::new(config, arguments, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::TestRail(source.with_limits(source_limits))
            }
            "qtest" => {
                let config = QtestConnectorConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments =
                    QtestIndexArguments::parse(&parameters.arguments).map_err(|e| refused(&e))?;
                let source = QtestSource::new(config, arguments, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::Qtest(source.with_limits(source_limits))
            }
            "xray_cloud" => {
                let config = XrayConnectorConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments =
                    XrayIndexArguments::parse(&parameters.arguments).map_err(|e| refused(&e))?;
                let source = XraySource::new(config, arguments, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::Xray(source.with_limits(source_limits))
            }
            "zephyr_scale" => {
                let config = ZephyrScaleConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments = ZephyrScaleIndexArguments::parse(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source =
                    ZephyrScaleSource::new(config, arguments, allowlist.clone(), transport)
                        .map_err(|e| refused(&e))?;
                Self::ZephyrScale(source.with_limits(source_limits))
            }
            "zephyr_essential" => {
                let config = ZephyrEssentialConfig::parse(settings).map_err(|e| refused(&e))?;
                let source = ZephyrEssentialSource::new(config, allowlist.clone(), transport)
                    .map_err(|e| refused(&e))?;
                Self::ZephyrEssential(source.with_limits(source_limits))
            }
            "zephyr_enterprise" => {
                let config = ZephyrEnterpriseConfig::parse(settings).map_err(|e| refused(&e))?;
                let arguments = ZephyrEnterpriseIndexArguments::parse(&parameters.arguments)
                    .map_err(|e| refused(&e))?;
                let source =
                    ZephyrEnterpriseSource::new(config, arguments, allowlist.clone(), transport)
                        .map_err(|e| refused(&e))?;
                Self::ZephyrEnterprise(source.with_limits(source_limits))
            }
            // The tests' in-memory toolkit: the documents the test staged.
            #[cfg(test)]
            "fixture" => match super::tests::take_fixture_source(parameters) {
                Some(source) => source,
                None => return Err(SourceBuildError("no fixture documents staged".to_owned())),
            },
            other => {
                return Err(SourceBuildError(format!(
                    "the {other} toolkit is not indexed by this worker yet (ADR-0030 I1–I3 serve {})",
                    SERVED_TOOLKIT_TYPES.join(", ")
                )));
            }
        };
        Ok(source)
    }

    /// What this run keeps of the listing: the `filter` of the toolkit
    /// type, except for the tests' in-memory toolkit, which brings its own.
    pub(crate) fn scoped(&self, filter: PathFilter) -> PathFilter {
        match self {
            #[cfg(test)]
            Self::Fixture(_, own) => own.clone(),
            _ => filter,
        }
    }

    /// Whether this source's documents are repository files, which carry the
    /// SDK's `commit_hash` (see [`commit_hash`]).
    pub(crate) fn hashes_commit(&self) -> bool {
        match self {
            Self::GitHub(_) | Self::GitLab(_) | Self::Bitbucket(_) | Self::AdoRepos(_) => true,
            // The tests' toolkit is a repository when a document says so.
            #[cfg(test)]
            Self::Fixture(documents, _) => documents
                .iter()
                .any(|document| document.metadata.contains_key("fixture_hash_commit")),
            _ => false,
        }
    }

    /// What the connector asks of the run for `reference`'s pictures
    /// (ADR-0030 I4, PR-C2): `None` unless the toolkit's picture setting
    /// (`process_images`, `bins_with_llm`) is on for this document.
    pub(crate) fn picture_step(&self, reference: &DocumentRef) -> Option<PictureStep> {
        match self {
            Self::AdoWiki(source) => source.picture_step(&reference.key),
            Self::AdoBoards(source) => source.picture_step(&reference.key),
            Self::Jira(source) => source.picture_step(&reference.key),
            Self::Confluence(source) => source.picture_step(reference),
            #[cfg(test)]
            Self::Fixture(documents, _) => documents
                .iter()
                .find(|document| document.reference.key == reference.key)
                .and_then(|document| document.metadata.get("fixture_pictures"))
                .map(|spec| PictureStep::Inline {
                    prompt: spec
                        .get("prompt")
                        .and_then(Value::as_str)
                        .map(ToOwned::to_owned),
                }),
            _ => None,
        }
    }

    /// Whether a fetched document has anything a picture could be read
    /// for. Asked before the run's model is waited for, so a document
    /// without a picture link neither waits nor is counted.
    pub(crate) fn may_have_pictures(
        &self,
        key: &str,
        bytes: &[u8],
        context: Option<&Value>,
    ) -> bool {
        match self {
            Self::AdoWiki(source) => source.may_have_pictures(bytes),
            Self::AdoBoards(source) => source.may_have_pictures(context),
            Self::Jira(source) => source.may_have_pictures(key, bytes),
            #[cfg(test)]
            Self::Fixture(documents, _) => documents.iter().any(|document| {
                document.reference.key == key && document.metadata.contains_key("fixture_pictures")
            }),
            _ => false,
        }
    }

    /// The pictures a fetched document links, read through the connector's
    /// own authenticated transport; `None` when there is nothing to read.
    pub(crate) async fn pictures(
        &self,
        key: &str,
        bytes: &[u8],
        context: Option<&Value>,
    ) -> Option<PictureBatch> {
        match self {
            Self::AdoWiki(source) => source.pictures(key, bytes).await,
            Self::AdoBoards(source) => source.pictures(key, bytes, context).await,
            Self::Jira(source) => source.pictures(key, bytes).await,
            #[cfg(test)]
            Self::Fixture(documents, _) => {
                let spec = documents
                    .iter()
                    .find(|document| document.reference.key == key)?
                    .metadata
                    .get("fixture_pictures")?
                    .clone();
                Some(fixture_batch(&spec))
            }
            _ => None,
        }
    }

    /// Tell the connector which document versions the index holds, before
    /// it lists. Only the boards connector uses them (to skip re-reading
    /// the comments of a work item that has not moved).
    pub(crate) fn remember_versions(&self, versions: BTreeMap<String, String>) {
        if let Self::AdoBoards(source) = self {
            source.remember_versions(versions);
        }
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
            Self::Artifact(source, folder) => source.list().await.map(|mut refs| {
                for reference in &mut refs {
                    reference.key = full_artifact_key(folder, &reference.key);
                }
                refs
            }),
            Self::Confluence(source) => source.list().await,
            Self::Jira(source) => source.list().await,
            Self::SharePoint(source) => source.list().await,
            Self::AdoWiki(source) => source.list().await,
            Self::AdoBoards(source) => source.list().await,
            Self::AdoPlans(source) => source.list().await,
            Self::Figma(source) => source.list().await,
            Self::TestRail(source) => source.list().await,
            Self::Qtest(source) => source.list().await,
            Self::Xray(source) => source.list().await,
            Self::ZephyrScale(source) => source.list().await,
            Self::ZephyrEssential(source) => source.list().await,
            Self::ZephyrEnterprise(source) => source.list().await,
            #[cfg(test)]
            Self::Fixture(documents, _) => Ok(documents
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
            Self::GitHub(source) => source.fetch(key).await.map(with_code_paths),
            Self::GitLab(source) => source.fetch(key).await.map(with_code_paths),
            Self::Bitbucket(source) => source.fetch(key).await.map(with_code_paths),
            Self::AdoRepos(source) => source.fetch(key).await.map(with_code_paths),
            Self::Artifact(source, folder) => {
                let relative = if folder.is_empty() {
                    key
                } else {
                    key.strip_prefix(folder.as_str())
                        .and_then(|rest| rest.strip_prefix('/'))
                        .ok_or_else(|| SourceError::NotFound(key.to_owned()))?
                };
                let mut document = source.fetch(relative).await?;
                key.clone_into(&mut document.reference.key);
                Ok(document)
            }
            Self::Confluence(source) => source.fetch(key).await,
            Self::Jira(source) => source.fetch(key).await,
            Self::SharePoint(source) => source.fetch(key).await,
            Self::AdoWiki(source) => source.fetch(key).await,
            Self::AdoBoards(source) => source.fetch(key).await,
            Self::AdoPlans(source) => source.fetch(key).await,
            Self::Figma(source) => source.fetch(key).await,
            Self::TestRail(source) => source.fetch(key).await,
            Self::Qtest(source) => source.fetch(key).await,
            Self::Xray(source) => source.fetch(key).await,
            Self::ZephyrScale(source) => source.fetch(key).await,
            Self::ZephyrEssential(source) => source.fetch(key).await,
            Self::ZephyrEnterprise(source) => source.fetch(key).await,
            #[cfg(test)]
            Self::Fixture(documents, _) => documents
                .iter()
                .find(|document| document.reference.key == key)
                .cloned()
                .ok_or_else(|| SourceError::NotFound(key.to_owned()))
                .and_then(|document| {
                    // A listed document whose fetch fails (the test marks it
                    // with this metadata key).
                    if document.metadata.contains_key("fixture_rate_limited") {
                        Err(SourceError::RateLimited(format!(
                            "fixture: '{key}' stayed rate limited past the backoff bound"
                        )))
                    } else if document.metadata.contains_key("fixture_unfetchable") {
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

/// The toolkit types whose documents are repository files: the SDK's
/// `CodeIndexerToolkit` family.
const GIT_FAMILIES: [&str; 4] = ["github", "gitlab", "bitbucket", "ado_repos"];

/// The SDK's code metadata on a repository file: `file_path` and `filename`
/// (the document's identity in the SDK, and what the code search output
/// prints), both the object key. A key the connector already set stays. The
/// third key, `commit_hash`, hashes the whole text and is added where the
/// text is already being read off the async executor ([`commit_hash`]).
pub(super) fn with_code_paths(mut document: Document) -> Document {
    let path = Value::String(document.reference.key.clone());
    document
        .metadata
        .entry("filename".to_owned())
        .or_insert_with(|| path.clone());
    document
        .metadata
        .entry("file_path".to_owned())
        .or_insert(path);
    document
}

/// The SDK's `commit_hash` of a repository file: the SHA-256 of the file's
/// text, not a git commit (`CodeIndexerToolkit.loader`). The SDK decodes with
/// `errors="ignore"` and hashes the text: valid UTF-8 hashes as it is (a real
/// U+FFFD stays), invalid sequences drop. Nothing is copied. CPU work: call
/// it from a blocking thread.
pub(super) fn commit_hash(bytes: &[u8]) -> String {
    use sha2::{Digest as _, Sha256};
    let mut hasher = Sha256::new();
    for chunk in bytes.utf8_chunks() {
        hasher.update(chunk.valid().as_bytes());
    }
    hasher
        .finalize()
        .iter()
        .fold(String::new(), |mut hex, byte| {
            let _ = std::fmt::Write::write_fmt(&mut hex, format_args!("{byte:02x}"));
            hex
        })
}

/// An artifact object's full key from the key relative to the folder.
fn full_artifact_key(folder: &str, relative: &str) -> String {
    if folder.is_empty() {
        relative.to_owned()
    } else {
        format!("{folder}/{relative}")
    }
}

/// The artifact toolkit's `folder` argument: the prefix to list under.
fn artifact_folder(arguments: &serde_json::Map<String, Value>) -> Result<String, String> {
    match arguments.get("folder") {
        None | Some(Value::Null) => Ok(String::new()),
        Some(Value::String(folder)) => Ok(folder.trim().trim_matches('/').to_owned()),
        Some(_) => Err("`folder` must be a string".to_owned()),
    }
}

fn pattern_list(
    arguments: &serde_json::Map<String, Value>,
    key: &str,
) -> Result<Vec<String>, String> {
    match arguments.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| match item {
                Value::String(pattern) => Ok(pattern.clone()),
                _ => Err(format!("`{key}` must hold strings")),
            })
            .collect(),
        Some(_) => Err(format!("`{key}` must be a list of strings")),
    }
}

/// The SDK's `ArtifactToolkit._fnmatch_nocase`: `fnmatch` of the lower-cased
/// key against the lower-cased pattern. The pattern is used as given (a bare
/// `pdf` is not `*.pdf`) and meets the whole key, folder included.
fn matches_nocase(key: &str, pattern: &str) -> bool {
    fnmatch(&pattern.to_lowercase(), &key.to_lowercase())
}

/// What a run keeps of the connector's listing: the `index_data` whitelist
/// and blacklist, and per toolkit family the SDK's own filters.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(crate) struct PathFilter {
    whitelist: Vec<String>,
    blacklist: Vec<String>,
    /// The code families' `skip_unsupported_extensions` (default true): only
    /// the SDK's [`SUPPORTED_EXTENSIONS`] are indexed.
    supported_only: bool,
    /// The artifact toolkit's folder, normalized once (`""` for the whole
    /// bucket). It scopes the listing only: documents are keyed by the full
    /// object key, and every filter meets that same key.
    folder: String,
    /// The artifact toolkit's `include_extensions` and `skip_extensions`.
    include: Vec<String>,
    skip: Vec<String>,
    /// Whether an EMPTY listing from the connector is a complete answer. The
    /// SDK's only attesting toolkit is the artifact one over the whole
    /// bucket; every other empty listing over a populated index is a failed
    /// run, not an instruction to delete (EL-6232).
    attests_empty: bool,
}

impl PathFilter {
    #[must_use]
    pub(crate) fn new(whitelist: &[String], blacklist: &[String]) -> Self {
        Self {
            whitelist: whitelist.to_vec(),
            blacklist: blacklist.to_vec(),
            ..Self::default()
        }
    }

    /// A filter with the family-specific rules set directly, for the tests'
    /// in-memory toolkit (the production way in is [`Self::for_run`]).
    #[cfg(test)]
    #[must_use]
    pub(crate) fn for_test(
        whitelist: &[String],
        blacklist: &[String],
        supported_only: bool,
        attests_empty: bool,
    ) -> Self {
        Self {
            supported_only,
            attests_empty,
            ..Self::new(whitelist, blacklist)
        }
    }

    /// The filter of one run of toolkit `toolkit_type` with `parameters`.
    ///
    /// # Errors
    ///
    /// An argument of the wrong type (`skip_unsupported_extensions`,
    /// `include_extensions`, `skip_extensions`, `folder`), with the reason.
    pub(crate) fn for_run(
        toolkit_type: &str,
        parameters: &IndexParameters,
    ) -> Result<Self, String> {
        let mut filter = Self::new(&parameters.whitelist, &parameters.blacklist);
        let arguments = &parameters.arguments;
        if GIT_FAMILIES.contains(&toolkit_type) {
            filter.supported_only = match arguments.get("skip_unsupported_extensions") {
                None | Some(Value::Null) => true,
                Some(Value::Bool(flag)) => *flag,
                Some(_) => {
                    return Err("`skip_unsupported_extensions` must be a boolean".to_owned());
                }
            };
        }
        if toolkit_type == "artifact" {
            let folder = artifact_folder(arguments)?;
            filter.include = pattern_list(arguments, "include_extensions")?;
            filter.skip = pattern_list(arguments, "skip_extensions")?;
            filter.attests_empty = folder.is_empty();
            filter.folder = folder;
        }
        Ok(filter)
    }

    /// The artifact folder the run lists under (`""` for the bucket).
    #[must_use]
    pub(crate) fn artifact_folder(&self) -> &str {
        &self.folder
    }

    /// Whether the connector's empty listing may delete a populated index.
    #[must_use]
    pub(crate) fn attests_empty(&self) -> bool {
        self.attests_empty
    }

    /// Whether the family's file-type allowlist (the default
    /// `skip_unsupported_extensions`) excludes a document key. Such a stored
    /// document is out of scope for a run, never "vanished".
    #[must_use]
    pub(crate) fn excluded_by_file_type(&self, path: &str) -> bool {
        self.supported_only && !has_supported_extension(path)
    }

    /// Whether a document path is indexed: on the whitelist (or no
    /// whitelist) and not on the blacklist, by the SDK's rule, and per
    /// family the SDK's extension allowlist or artifact patterns.
    #[must_use]
    pub(crate) fn keeps(&self, path: &str) -> bool {
        let listed = |patterns: &[String]| {
            patterns
                .iter()
                .any(|pattern| fnmatch(pattern, path) || path.ends_with(&format!(".{pattern}")))
        };
        if !((self.whitelist.is_empty() || listed(&self.whitelist)) && !listed(&self.blacklist)) {
            return false;
        }
        if self.excluded_by_file_type(path) {
            return false;
        }
        if !self.include.is_empty() || !self.skip.is_empty() {
            // Skip patterns first, then the include list when there is one.
            if self
                .skip
                .iter()
                .any(|pattern| matches_nocase(path, pattern))
            {
                return false;
            }
            if !self.include.is_empty()
                && !self
                    .include
                    .iter()
                    .any(|pattern| matches_nocase(path, pattern))
            {
                return false;
            }
        }
        true
    }
}

/// The tests' connector picture batch, from a document's
/// `fixture_pictures` metadata: `template`, `pictures` (`tag`, optional
/// `prompt`, `before`, `after`, `fallback`) and `skipped` (reason to count).
#[cfg(test)]
fn fixture_batch(spec: &Value) -> PictureBatch {
    use elitea_connectors::pictures::{SourcePicture, token};
    let text = |value: &Value, name: &str| {
        value
            .get(name)
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned()
    };
    let mut batch = PictureBatch {
        template: text(spec, "template"),
        ..PictureBatch::default()
    };
    for (index, picture) in spec["pictures"]
        .as_array()
        .into_iter()
        .flatten()
        .enumerate()
    {
        let mut bytes = b"\x89PNG\r\n\x1a\n".to_vec();
        bytes.extend_from_slice(text(picture, "tag").as_bytes());
        let bytes = bytes::Bytes::from(bytes);
        batch.pictures.push(SourcePicture {
            token: token(index),
            name: text(picture, "tag"),
            bytes,
            prompt: picture
                .get("prompt")
                .and_then(Value::as_str)
                .map(ToOwned::to_owned),
            before: text(picture, "before"),
            after: text(picture, "after"),
            fallback: text(picture, "fallback"),
            escapes: &[],
        });
    }
    for (reason, count) in spec["skipped"].as_object().into_iter().flatten() {
        let reason: &'static str = match reason.as_str() {
            "fetch_failed" => "fetch_failed",
            "rate_limited" => "rate_limited",
            "unsupported_type" => "unsupported_type",
            "too_large" => "too_large",
            _ => "unreadable",
        };
        for _ in 0..count.as_u64().unwrap_or(0) {
            batch.skip(reason);
        }
    }
    batch
}

#[cfg(test)]
mod tests {
    use super::*;

    /// An artifact client nothing answers, for the tests that only build a
    /// source (its channel needs a runtime to exist; none is driven).
    fn unreachable_artifacts() -> (tokio::runtime::Runtime, ArtifactClient) {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime");
        let channel = {
            let _entered = runtime.enter();
            tonic::transport::Endpoint::from_static("http://127.0.0.1:9").connect_lazy()
        };
        (runtime, ArtifactClient::new(channel))
    }

    /// The served families have one source of truth, shared with the Go
    /// `indexcatalog` and the agent-runtime tests (review round 1).
    #[test]
    fn the_served_families_match_the_shared_fixture() {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../libs/rust/agent-runtime/src/toolkits/families/index_served_families.json");
        let fixture: Value =
            serde_json::from_str(&std::fs::read_to_string(path).expect("the fixture is readable"))
                .expect("the fixture is JSON");
        let expected: Vec<&str> = fixture["families"]
            .as_array()
            .expect("families")
            .iter()
            .map(|family| family.as_str().expect("a name"))
            .collect();
        let mut served: Vec<&str> = SERVED_TOOLKIT_TYPES.into_iter().collect();
        served.sort_unstable();
        assert_eq!(served, expected);
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
            toolkit_type: "aha".to_owned(),
            toolkit_name: "ideas".to_owned(),
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
        let transport: Arc<dyn Transport> = Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("client"),
        );
        let (_runtime, artifacts) = unreachable_artifacts();
        let platform = PlatformAccess {
            artifacts: &artifacts,
            bearer: "pat",
            project_id: "1",
        };
        let error = IndexSource::build(
            &configuration,
            &parameters,
            &PathFilter::for_run(&configuration.toolkit_type, &parameters).expect("a valid filter"),
            &platform,
            &HostAllowlist::parse(Some("*")),
            Arc::clone(&transport),
            IndexRunLimits::default(),
        )
        .err()
        .expect("refused");
        assert!(error.0.contains("aha"), "{error}");
        assert!(error.0.contains("ADR-0030 I1–I3"), "{error}");
        assert!(error.0.contains("confluence"), "{error}");
        assert!(error.0.contains("ado_plans"), "{error}");
    }

    /// The Atlassian families are built from the toolkit settings and the
    /// `index_data` arguments; a refused configuration or argument names the
    /// toolkit type and the connector's data-free reason.
    #[test]
    fn the_atlassian_families_are_built_or_refused_with_a_reason() {
        let transport: Arc<dyn Transport> = Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("client"),
        );
        let (_runtime, artifacts) = unreachable_artifacts();
        let platform = PlatformAccess {
            artifacts: &artifacts,
            bearer: "pat",
            project_id: "1",
        };
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
                &PathFilter::for_run(&configuration.toolkit_type, &parameters)
                    .expect("a valid filter"),
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

    /// The artifact connector lists under the SDK's `folder`; a folder that
    /// is not a string, or not a valid key, is refused.
    #[test]
    fn the_artifact_connector_is_scoped_to_the_folder_argument() {
        let transport: Arc<dyn Transport> = Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("client"),
        );
        let (_runtime, artifacts) = unreachable_artifacts();
        let platform = PlatformAccess {
            artifacts: &artifacts,
            bearer: "pat",
            project_id: "1",
        };
        let build = |arguments: Value| {
            let configuration = ToolkitConfiguration {
                toolkit_type: "artifact".to_owned(),
                toolkit_name: "files".to_owned(),
                settings: serde_json::json!({"bucket": "docs"})
                    .as_object()
                    .cloned()
                    .unwrap_or_default(),
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
            let filter = PathFilter::for_run(&configuration.toolkit_type, &parameters)
                .map_err(SourceBuildError)?;
            IndexSource::build(
                &configuration,
                &parameters,
                &filter,
                &platform,
                &HostAllowlist::parse(Some("*")),
                Arc::clone(&transport),
                IndexRunLimits::default(),
            )
        };
        assert!(matches!(
            build(serde_json::json!({})),
            Ok(IndexSource::Artifact(..))
        ));
        assert!(matches!(
            build(serde_json::json!({"folder": "/reports/2026/"})),
            Ok(IndexSource::Artifact(..))
        ));
        let error = build(serde_json::json!({"folder": 7}))
            .err()
            .expect("refused");
        assert!(error.0.contains("`folder` must be a string"), "{error}");
        let error = build(serde_json::json!({"folder": "a/../b"}))
            .err()
            .expect("refused");
        assert!(error.0.contains("folder"), "{error}");
    }

    /// Figma is built from the token and the `index_data` arguments; a missing
    /// token or file, or an egress list without `api.figma.com`, is refused
    /// with the connector's data-free reason.
    #[test]
    fn figma_is_built_or_refused_with_a_reason() {
        let transport: Arc<dyn Transport> = Arc::new(
            elitea_connectors::reqwest012::ReqwestTransport::hardened(
                &elitea_connectors::github::client::CLIENT_POLICY,
            )
            .expect("client"),
        );
        let (_runtime, artifacts) = unreachable_artifacts();
        let platform = PlatformAccess {
            artifacts: &artifacts,
            bearer: "pat",
            project_id: "1",
        };
        let build = |settings: Value, arguments: Value, allowlist: &str| {
            let configuration = ToolkitConfiguration {
                toolkit_type: "figma".to_owned(),
                toolkit_name: "designs".to_owned(),
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
                &PathFilter::for_run(&configuration.toolkit_type, &parameters)
                    .expect("a valid filter"),
                &platform,
                &HostAllowlist::parse(Some(allowlist)),
                Arc::clone(&transport),
                IndexRunLimits::default(),
            )
        };
        let settings = serde_json::json!({"figma_configuration": {"token": "figd_fixture"}});
        let arguments = serde_json::json!({"urls_or_file_keys": "https://www.figma.com/design/K1/x?node-id=1-2"});
        assert!(matches!(
            build(settings.clone(), arguments.clone(), "api.figma.com"),
            Ok(IndexSource::Figma(_))
        ));
        let error = build(settings.clone(), arguments.clone(), "example.com")
            .err()
            .expect("refused");
        assert!(error.0.contains("egress allowlist"), "{error}");
        let error = build(serde_json::json!({}), arguments, "*")
            .err()
            .expect("no token");
        assert!(
            error.0.starts_with("the figma toolkit cannot be indexed:"),
            "{error}"
        );
        assert!(error.0.contains("figma_configuration.token"), "{error}");
        let error = build(settings, serde_json::json!({}), "*")
            .err()
            .expect("no file");
        assert!(error.0.contains("urls_or_file_keys"), "{error}");
        assert!(!error.0.contains("figd_fixture"));
    }

    /// Live defect: the artifact source is the platform's own artifact
    /// store and must be reached with the shipped (public-SaaS-only)
    /// allowlist, through the platform listener's `ArtifactObjectService`
    /// only and never the connectors' transport (#1359 U5).
    #[tokio::test]
    async fn the_artifact_source_reaches_the_platform_under_the_default_allowlist() {
        use std::sync::Mutex;

        use elitea_platform_rpc::testing::{FakeArtifactStore, serve};

        struct Recording(Mutex<Vec<String>>);
        #[async_trait::async_trait]
        impl Transport for Recording {
            async fn execute(
                &self,
                request: elitea_connectors::transport::Request,
            ) -> Result<
                elitea_connectors::transport::Response,
                elitea_connectors::transport::TransportError,
            > {
                self.0.lock().expect("lock").push(request.url().to_string());
                Err(elitea_connectors::transport::TransportError::other())
            }
        }

        let default_allowlist = HostAllowlist::parse(Some(
            "api.github.com,gitlab.com,*.atlassian.net,api.figma.com",
        ));
        let configuration = ToolkitConfiguration {
            toolkit_type: "artifact".to_owned(),
            toolkit_name: "files".to_owned(),
            settings: serde_json::json!({"bucket": "docs"})
                .as_object()
                .cloned()
                .unwrap_or_default(),
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
        let filter = PathFilter::for_run("artifact", &parameters).expect("a valid filter");
        let store = FakeArtifactStore::new()
            .grant("pat", 7)
            .with_object(7, "docs", "a.md", b"alpha");
        let artifacts = ArtifactClient::new(serve(Some(store.clone()), None));
        let connectors_transport = Arc::new(Recording(Mutex::new(Vec::new())));
        let platform = PlatformAccess {
            artifacts: &artifacts,
            bearer: "pat",
            project_id: "7",
        };
        let source = IndexSource::build(
            &configuration,
            &parameters,
            &filter,
            &platform,
            &default_allowlist,
            connectors_transport.clone(),
            IndexRunLimits::default(),
        )
        .unwrap_or_else(|error| panic!("built: {error}"));
        let listed = source.list().await.expect("listed through the platform");
        assert_eq!(listed.len(), 1);
        let calls = store.calls();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].op, "ListObjects");
        assert_eq!(calls[0].bucket, "docs");
        assert_eq!(calls[0].project_id, 7);
        assert_eq!(calls[0].bearer.as_deref(), Some("pat"));
        assert!(
            connectors_transport.0.lock().expect("lock").is_empty(),
            "the connectors' transport carried a platform call"
        );
    }
}
