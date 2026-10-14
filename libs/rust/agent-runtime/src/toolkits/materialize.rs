//! Claim-materialized configured toolsets for a direct ADK `LlmAgent`.
//!
//! Main freezes toolkit identity and redeems schema-declared secrets before the
//! worker receives the request. This boundary consumes only that immutable
//! request snapshot, applies one deployment-policy generation, and constructs
//! native ADK toolsets. MCP and nested applications have separate authority
//! owners and therefore never fall through this map.
//!
//! The `artifact` family (#906) is the one entry here whose authority is NOT in
//! the snapshot: there is no third-party endpoint and no credential to redeem,
//! only this platform's own storage reached under the live execution CLAIM. So
//! it is materialized only when a caller LENDS that claim — see
//! `materialize_configured_toolsets_with_artifact_authority` — and where none
//! is in scope (pipelines, nested applications) it stays exactly what it was:
//! skipped with `agent_toolkit_skipped`, which is the honest answer for a
//! runtime that cannot serve it there.
//!
//! The index tools (ADR-0030 I1) are the second lent authority: for an
//! indexing family whose frozen toolkit carries Main's `indexes` catalog, the
//! toolset is composed of the family's tools and the five index tools, acting
//! under an `IndexToolAuthority` the host lends
//! (`materialize_configured_toolsets_with_authorities`). No catalog (the
//! Python indexing path) means no index tools; a catalog with the authority
//! withheld ([`IndexToolSupport::Withheld`]) means they are left out, logged
//! with the reason the host states. Only where the index toolset IS composed
//! is an index-only `selected_tools` admissible — it then selects none of the
//! family's own tools and the family is not built at all; elsewhere every
//! family refuses it as the unsupported selection it always was.

use std::fmt;
use std::sync::Arc;

use adk_core::Toolset;

use super::DelegatedAuthorizationCatalog;
use super::families::UnsupportedSetting;
use super::families::artifact::ArtifactToolAuthority;
use super::families::bigquery;
use super::families::confluence;
use super::families::index_tools::{
    CompositeToolset, IndexCatalog, IndexToolSupport, IndexToolsWithheld, IndexToolsetErrorCode,
    build_index_toolset, is_index_family, selection_is_index_only,
};
use super::families::jira;
use super::families::qtest;
#[cfg(feature = "toolkit-sql")]
use super::families::sql;
use super::families::testio;
use super::families::testrail;
use super::families::xray_cloud;
use super::families::{
    aha, artifact, azure, azure_search, bitbucket, carrier, elastic, figma, gcp, github, gitlab,
    gitlab_org, google_places, keycloak, kubernetes, openapi, postman, rally, report_portal,
    salesforce, service_now, sharepoint, slack, sonar, yagmail, zephyr, zephyr_squad,
};
#[cfg(feature = "toolkit-office")]
use super::families::{docx, pptx, xlsx};
use super::family_error::family_error;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::{AdmittedToolSnapshot, FrozenToolKind, FrozenToolReference};

const MAX_AGENT_TOOLSETS: usize = 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ToolsetMaterializationErrorCode {
    InvalidConfiguration,
    UnsupportedToolkit,
    ResourceExhausted,
    DependencyUnavailable,
}

#[derive(Clone, Copy)]
pub struct ToolsetMaterializationError {
    code: ToolsetMaterializationErrorCode,
    /// Set when the toolkit's configuration asks for a setting this runtime
    /// deliberately refuses (#1207 review round 2). Within one
    /// materialization pass such a toolkit is collected as a
    /// [`RefusedToolkit`]; a caller that cannot report one receives it as an
    /// `UnsupportedToolkit` error carrying the reason.
    refusal: Option<UnsupportedSetting>,
}

impl ToolsetMaterializationError {
    pub(super) const fn new(code: ToolsetMaterializationErrorCode) -> Self {
        Self {
            code,
            refusal: None,
        }
    }

    pub(super) const fn refused(setting: UnsupportedSetting) -> Self {
        Self {
            code: ToolsetMaterializationErrorCode::UnsupportedToolkit,
            refusal: Some(setting),
        }
    }

    #[must_use]
    pub const fn code(self) -> ToolsetMaterializationErrorCode {
        self.code
    }

    /// The refused setting, as a sentence that carries no configuration
    /// value; `None` for every other failure.
    #[must_use]
    pub const fn refusal_reason(self) -> Option<&'static str> {
        match self.refusal {
            Some(setting) => Some(setting.reason()),
            None => None,
        }
    }
}

impl fmt::Debug for ToolsetMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ToolsetMaterializationError")
            .field("code", &self.code)
            .field("refusal", &self.refusal)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ToolsetMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        if let Some(setting) = self.refusal {
            formatter.write_str("the frozen toolkit configuration is refused: ")?;
            return formatter.write_str(setting.reason());
        }
        formatter.write_str(match self.code {
            ToolsetMaterializationErrorCode::DependencyUnavailable => {
                "the toolkit specification could not be retrieved"
            }
            ToolsetMaterializationErrorCode::InvalidConfiguration => {
                "the frozen toolkit configuration is invalid"
            }
            ToolsetMaterializationErrorCode::UnsupportedToolkit => {
                "the frozen toolkit requires a runtime authority that is not available"
            }
            ToolsetMaterializationErrorCode::ResourceExhausted => {
                "the frozen toolkit set exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ToolsetMaterializationError {}

/// A configured toolkit left out of the agent because its configuration asks
/// for a setting this runtime refuses (#1207 review round 2).
///
/// Before, such a toolkit was skipped exactly like an unported family — one
/// warning in the worker log — while the capability snapshot listed it as
/// supported, so the user saw an enabled toolkit that did nothing. The
/// ordinary agent path now names every refused toolkit in the run's opening
/// notice ([`refused_toolkits_notice_text`]), in the same place and shape as
/// the skipped internal tools (#866) and application children (#973).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RefusedToolkit {
    toolkit_name: String,
    toolkit_type: String,
    reason: &'static str,
}

impl RefusedToolkit {
    fn new(reference: &FrozenToolReference<'_>, setting: UnsupportedSetting) -> Self {
        Self {
            toolkit_name: bounded_label(reference.toolkit_name()),
            toolkit_type: bounded_label(reference.tool_type()),
            reason: setting.reason(),
        }
    }

    #[must_use]
    pub fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    #[must_use]
    pub fn toolkit_type(&self) -> &str {
        &self.toolkit_type
    }

    #[must_use]
    pub const fn reason(&self) -> &'static str {
        self.reason
    }
}

/// The longest toolkit name or type a notice repeats, in characters. Both
/// come from stored rows the runtime does not otherwise bound.
const MAX_NOTICE_LABEL_CHARS: usize = 128;

fn bounded_label(value: &str) -> String {
    value
        .chars()
        .filter(|character| !character.is_control())
        .take(MAX_NOTICE_LABEL_CHARS)
        .collect()
}

/// One deterministic line per refused toolkit, naming the toolkit and the
/// reason; `None` when nothing was refused. The set decides the text, never
/// the order the version listed the toolkits in.
#[must_use]
pub fn refused_toolkits_notice_text(refused: &[RefusedToolkit]) -> Option<String> {
    if refused.is_empty() {
        return None;
    }
    let mut lines = refused
        .iter()
        .map(|toolkit| {
            format!(
                "toolkit '{}' ({}) is not enabled on this worker: {}",
                toolkit.toolkit_name, toolkit.toolkit_type, toolkit.reason
            )
        })
        .collect::<Vec<_>>();
    lines.sort_unstable();
    lines.dedup();
    Some(lines.join("\n"))
}

/// The configured toolsets one pass built, and the toolkits it refused.
pub struct ConfiguredToolsets {
    pub toolsets: Vec<Arc<dyn Toolset>>,
    pub delegated_authorization: DelegatedAuthorizationCatalog,
    /// Toolkits left out for a refused setting, for the caller to report.
    pub refused: Vec<RefusedToolkit>,
}

pub async fn materialize_configured_toolsets_with_tokens_and_authorization(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    materialize_configured_toolsets_with_artifact_authority(
        snapshot,
        policy,
        delegated_tokens,
        None,
    )
    .await
}

/// The same materialization, with the live execution claim LENT to the one
/// family that needs it.
///
/// `artifacts` is an `Option` rather than a required argument because only the
/// ordinary agent path holds a claim that outlives assembly. A caller with
/// none passes `None` and gets exactly what it got before #906: an artifact
/// toolkit skipped with a warning naming it. That is deliberately not an
/// error — the toolkit is a real capability of the product that this runtime
/// cannot serve in that position, and refusing the whole profile would turn
/// one unavailable tool into an agent that stops answering.
///
/// These callers (pipelines, nested applications, direct tool calls) have no
/// opening notice to name a refused toolkit in, so a refused setting is an
/// `UnsupportedToolkit` ERROR here, carrying the reason — visible as
/// "configuration type is not supported" rather than a toolkit that silently
/// does nothing. The ordinary agent path uses
/// [`materialize_configured_toolsets_reporting_refusals`] instead.
pub async fn materialize_configured_toolsets_with_artifact_authority(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    materialize_configured_toolsets_with_authorities(
        snapshot,
        policy,
        delegated_tokens,
        artifacts,
        IndexToolSupport::Withheld(IndexToolsWithheld::NotServedOnThisPath),
    )
    .await
}

/// The same materialization, with both lent authorities: the claim for the
/// `artifact` family and the index tools' host for the indexing families
/// (ADR-0030 I1). Either may be withheld, with the consequences each family
/// documents: no artifact family (unless its index tools are composed, which
/// then stand alone), or no index tools.
pub async fn materialize_configured_toolsets_with_authorities(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
    index_tools: IndexToolSupport<'_>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    let mut refusal = None;
    let materialized = materialize_all(
        snapshot,
        policy,
        delegated_tokens,
        artifacts,
        index_tools,
        &mut |_, setting| {
            refusal.get_or_insert(ToolsetMaterializationError::refused(setting));
        },
    )
    .await?;
    if let Some(error) = refusal {
        return Err(error);
    }
    Ok((materialized.toolsets, materialized.delegated_authorization))
}

/// The ordinary agent path: build every configured toolkit, and REPORT the
/// ones refused for a setting this runtime does not honour instead of
/// failing the agent, so the run can name them
/// ([`refused_toolkits_notice_text`]).
pub async fn materialize_configured_toolsets_reporting_refusals(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
    index_tools: IndexToolSupport<'_>,
) -> Result<ConfiguredToolsets, ToolsetMaterializationError> {
    let mut refused = Vec::new();
    let mut materialized = materialize_all(
        snapshot,
        policy,
        delegated_tokens,
        artifacts,
        index_tools,
        &mut |reference, setting| refused.push(RefusedToolkit::new(reference, setting)),
    )
    .await?;
    materialized.refused = refused;
    Ok(materialized)
}

async fn materialize_all(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
    index_tools: IndexToolSupport<'_>,
    on_refused: &mut (dyn FnMut(&FrozenToolReference<'_>, UnsupportedSetting) + Send),
) -> Result<ConfiguredToolsets, ToolsetMaterializationError> {
    if snapshot.len() > MAX_AGENT_TOOLSETS {
        return Err(resource_exhausted());
    }
    let mut toolsets = Vec::new();
    let mut delegated_authorization = DelegatedAuthorizationCatalog::default();
    for reference in snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Configured)
    {
        let index_toolset = index_toolset_for(reference, policy, index_tools)?;
        // With the index toolset composed, two toolkits have no family half
        // to build: one whose stored selection names index tools only (none
        // of the family's own tools are selected, so the family would refuse
        // the selection), and an `artifact` toolkit on a path that lends no
        // artifact authority (the family is skipped there; its index tools
        // are not). Without the index toolset both keep their old answer.
        //
        // A family half that is REFUSED (an unsupported setting, reported to
        // the run) or SKIPPED (a selection or family this runtime does not
        // serve) is left out alone: the toolkit's index tools, which depend
        // on none of the family's settings, are still served (review round
        // 2). Without an index toolset such a toolkit is omitted as before.
        let family = if index_toolset.is_some() && index_tools_stand_alone(reference, artifacts)? {
            None
        } else {
            match materialize(reference, policy, delegated_tokens, artifacts).await {
                Ok((toolset, authorization)) => {
                    delegated_authorization
                        .merge(authorization)
                        .map_err(|()| invalid_configuration())?;
                    Some(toolset)
                }
                Err(ToolsetMaterializationError {
                    refusal: Some(setting),
                    ..
                }) => {
                    tracing::warn!(
                        event = "agent_toolkit_refused",
                        reason_code = "unsupported_setting",
                        toolkit_type = reference.tool_type(),
                        toolkit_id = reference.tool_id(),
                        reason = setting.reason(),
                        index_tools_kept = index_toolset.is_some(),
                        "agent toolkit configuration asks for a setting this runtime refuses; the toolkit's own tools were left out"
                    );
                    on_refused(reference, setting);
                    None
                }
                Err(error)
                    if error.code() == ToolsetMaterializationErrorCode::UnsupportedToolkit =>
                {
                    tracing::warn!(
                        event = "agent_toolkit_skipped",
                        reason_code = "unsupported_toolkit_family",
                        toolkit_type = reference.tool_type(),
                        toolkit_id = reference.tool_id(),
                        index_tools_kept = index_toolset.is_some(),
                        "agent toolkit is unavailable in this runtime and its own tools were omitted from the native toolsets"
                    );
                    None
                }
                Err(error) => return Err(error),
            }
        };
        let toolset: Arc<dyn Toolset> = match (family, index_toolset) {
            (Some(family), Some(index)) => Arc::new(CompositeToolset::new(
                reference.toolkit_name(),
                vec![family, Arc::new(index)],
            )),
            (Some(family), None) => family,
            (None, Some(index)) => Arc::new(CompositeToolset::new(
                reference.toolkit_name(),
                vec![Arc::new(index)],
            )),
            (None, None) => continue,
        };
        toolsets.push(toolset);
    }
    Ok(ConfiguredToolsets {
        toolsets,
        delegated_authorization,
        refused: Vec::new(),
    })
}

/// Whether, with the index toolset composed, this toolkit has no family
/// half: its stored selection is index-only, or it is an `artifact` toolkit
/// on a path lending no artifact authority (logged, since the family's own
/// tools are then left out while its index tools are served).
fn index_tools_stand_alone(
    reference: &FrozenToolReference<'_>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<bool, ToolsetMaterializationError> {
    let settings = reference.settings().ok_or_else(invalid_configuration)?;
    if selection_is_index_only(settings).map_err(|_| invalid_configuration())? {
        return Ok(true);
    }
    if reference.tool_type() == "artifact" && artifacts.is_none() {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "no_artifact_authority",
            toolkit_type = reference.tool_type(),
            toolkit_id = reference.tool_id(),
            "this path lends no artifact authority, so the artifact toolkit serves only its index tools"
        );
        return Ok(true);
    }
    Ok(false)
}

/// The index toolset of an indexing family whose frozen toolkit carries
/// Main's catalog, when the host lends an authority (ADR-0030 I1); `None`
/// otherwise — logged with the host's reason when a catalog is present and
/// the authority is withheld.
fn index_toolset_for(
    reference: &FrozenToolReference<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    index_tools: IndexToolSupport<'_>,
) -> Result<Option<adk_tool::BasicToolset>, ToolsetMaterializationError> {
    let Some(catalog) = reference
        .indexes()
        .filter(|_| is_index_family(reference.tool_type()))
    else {
        return Ok(None);
    };
    let authority = match index_tools {
        IndexToolSupport::Lent(authority) => authority,
        IndexToolSupport::Withheld(reason) => {
            if reason.is_warning() {
                tracing::warn!(
                    event = "agent_index_tools_unavailable",
                    reason_code = reason.reason_code(),
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "{}",
                    reason.message()
                );
            } else {
                tracing::info!(
                    event = "agent_index_tools_unavailable",
                    reason_code = reason.reason_code(),
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "{}",
                    reason.message()
                );
            }
            return Ok(None);
        }
    };
    // A catalog Main froze that this runtime cannot read is a disagreement
    // between the two freeze rules (both sides apply `indexcatalog`'s entry
    // rule and its 1024-entry cap), not a configuration the user can fix:
    // the toolkit's index tools are left out with a warning and the family's
    // own tools are served (review round 2).
    let catalog = match IndexCatalog::parse(catalog) {
        Ok(catalog) => catalog,
        Err(error) => {
            tracing::warn!(
                event = "agent_index_tools_unavailable",
                reason_code = "invalid_catalog",
                toolkit_type = reference.tool_type(),
                toolkit_id = reference.tool_id(),
                error = %error,
                "the toolkit's frozen index catalog is not readable; its index tools were left out"
            );
            return Ok(None);
        }
    };
    let settings = reference.settings().ok_or_else(invalid_configuration)?;
    // A toolkit without an id cannot be addressed by Main's index delete
    // flow, which `remove_index` runs through; Main freezes every configured
    // toolkit with one.
    let toolkit_id = reference.tool_id().ok_or_else(invalid_configuration)?;
    build_index_toolset(
        reference.toolkit_name(),
        toolkit_id,
        reference.tool_type(),
        settings,
        catalog,
        policy,
        authority,
    )
    .map(Some)
    .map_err(|error| match error.code() {
        IndexToolsetErrorCode::InvalidConfiguration => invalid_configuration(),
        IndexToolsetErrorCode::ResourceExhausted => resource_exhausted(),
    })
}

#[allow(
    clippy::too_many_lines,
    reason = "one dispatch table over the toolkit families; each arm is a call"
)]
async fn materialize(
    reference: &FrozenToolReference<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<(Arc<dyn Toolset>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    if reference.kind() != FrozenToolKind::Configured {
        return Err(unsupported_toolkit());
    }
    let settings = reference.settings().ok_or_else(invalid_configuration)?;
    let name = reference.toolkit_name();
    if reference.tool_type() == "artifact" {
        // No authority lent means no family: the toolkit is skipped with the
        // warning the caller above logs, not bound to tools that would fail on
        // every call.
        let authority = artifacts.ok_or_else(unsupported_toolkit)?;
        let config = artifact::config::ArtifactToolkitConfig::parse(settings)
            .map_err(|error| artifact_config_materialization_error(error.code()))?;
        let toolset = artifact::tools::build_artifact_toolset(name, &config, policy, authority)
            .map_err(|error| artifact_toolset_materialization_error(error.code()))?;
        return Ok((Arc::new(toolset), DelegatedAuthorizationCatalog::default()));
    }
    #[cfg(feature = "toolkit-office")]
    if matches!(reference.tool_type(), "pptx" | "docx" | "xlsx") {
        let toolset = materialize_office(reference.tool_type(), name, settings, policy, artifacts)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type() == "imagegen" {
        return materialize_imagegen(name, settings, policy, artifacts)
            .map(|toolset| (toolset, DelegatedAuthorizationCatalog::default()));
    }
    if matches!(
        reference.tool_type(),
        "aha" | "bigquery" | "bitbucket" | "carrier" | "confluence" | "figma" | "gitlab" | "jira"
    ) {
        let toolset = materialize_ported_saas(reference.tool_type(), name, settings, policy)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if matches!(
        reference.tool_type(),
        "azure"
            | "azure_search"
            | "elastic"
            | "gcp"
            | "github"
            | "gitlab_org"
            | "google_places"
            | "k8s"
            | "keycloak"
    ) {
        let toolset = materialize_a_to_k(reference.tool_type(), name, settings, policy)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type() == "openapi" {
        let remote = openapi::source::load(settings)
            .await
            .map_err(|error| match error {
                openapi::source::SourceError::Invalid => invalid_configuration(),
                openapi::source::SourceError::TooLarge => resource_exhausted(),
                openapi::source::SourceError::Unavailable => ToolsetMaterializationError::new(
                    ToolsetMaterializationErrorCode::DependencyUnavailable,
                ),
            })?;
        let config = match remote.as_ref() {
            Some(spec) => openapi::config::OpenApiToolkitConfig::parse_with_spec(
                name,
                settings,
                delegated_tokens,
                spec,
            ),
            None => openapi::config::OpenApiToolkitConfig::parse(name, settings, delegated_tokens),
        }
        .map_err(|error| openapi_materialization_error(error.code()))?
        .with_toolkit_id(reference.tool_id());
        let materialized = openapi::tools::build_openapi_toolset(name, config, policy)
            .map_err(|error| openapi_toolset_materialization_error(error.code()))?;
        return Ok((
            Arc::new(materialized.toolset),
            materialized.delegated_authorization,
        ));
    }
    if reference.tool_type() == "sharepoint" {
        let config =
            sharepoint::config::SharePointToolkitConfig::parse(name, settings, delegated_tokens)
                .map_err(|error| sharepoint_materialization_error(error.code()))?
                .with_toolkit_id(reference.tool_id());
        let materialized = sharepoint::tools::build_sharepoint_toolset(name, config, policy)
            .map_err(|error| sharepoint_toolset_materialization_error(error.code()))?;
        return Ok((
            Arc::new(materialized.toolset),
            materialized.delegated_authorization,
        ));
    }
    if let Some(toolset) = materialize_zephyr_rest(reference.tool_type(), name, settings, policy)? {
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type().starts_with("ado_") {
        let toolset = materialize_ado(reference.tool_type(), name, settings, policy)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if matches!(
        reference.tool_type(),
        "qtest" | "testio" | "testrail" | "xray_cloud"
    ) {
        let toolset = materialize_test_management(reference.tool_type(), name, settings, policy)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    let toolset = materialize_p_to_z(reference.tool_type(), name, settings, policy)?;
    Ok((toolset, DelegatedAuthorizationCatalog::default()))
}

/// The bearer-token Zephyr REST families (shared `zephyr_rest` transport);
/// `None` for any other type.
fn materialize_zephyr_rest(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Option<Arc<dyn Toolset>>, ToolsetMaterializationError> {
    use super::families::{zephyr_enterprise, zephyr_essential, zephyr_scale};
    let toolset = match tool_type {
        "zephyr_enterprise" => zephyr_enterprise::tools::build_zephyr_enterprise_toolset(
            name,
            zephyr_enterprise::config::ZephyrEnterpriseToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        ),
        "zephyr_essential" => zephyr_essential::tools::build_zephyr_essential_toolset(
            name,
            zephyr_essential::config::ZephyrEssentialToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        ),
        "zephyr_scale" => zephyr_scale::tools::build_zephyr_scale_toolset(
            name,
            zephyr_scale::config::ZephyrScaleToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        ),
        _ => return Ok(None),
    }
    .map_err(|error| family_error(error.code()))?;
    Ok(Some(Arc::new(toolset)))
}

/// The Azure DevOps families, which share one client (`families::ado`).
fn materialize_ado(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    use super::families::ado_boards;
    use super::families::ado_plans;
    use super::families::ado_repos;
    use super::families::ado_wiki;
    let toolset = match tool_type {
        "ado_boards" => ado_boards::tools::build_ado_boards_toolset(
            name,
            ado_boards::config::AdoBoardsToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "ado_plans" => ado_plans::tools::build_ado_plans_toolset(
            name,
            ado_plans::config::AdoPlansToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "ado_repos" => ado_repos::tools::build_ado_repos_toolset(
            name,
            ado_repos::config::AdoReposToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "ado_wiki" => ado_wiki::tools::build_ado_wiki_toolset(
            name,
            ado_wiki::config::AdoWikiToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

fn materialize_a_to_k(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        "azure" => azure::tools::build_azure_toolset(
            name,
            azure::config::AzureToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "azure_search" => azure_search::tools::build_azure_search_read_only_toolset(
            name,
            azure_search::config::AzureSearchToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "elastic" => elastic::tools::build_elastic_toolset(
            name,
            elastic::config::ElasticToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "gcp" => gcp::tools::build_gcp_toolset(
            name,
            gcp::config::GcpToolkitConfig::parse(settings).map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "github" => github::tools::build_github_read_only_toolset(
            name,
            github::config::GitHubToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|error| github_toolset_materialization_error(error.code()))?,
        "gitlab_org" => gitlab_org::tools::build_gitlab_org_toolset(
            name,
            gitlab_org::config::GitLabOrgToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "google_places" => google_places::tools::build_google_places_read_only_toolset(
            name,
            google_places::config::GooglePlacesToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "keycloak" => keycloak::tools::build_keycloak_toolset(
            name,
            keycloak::config::KeycloakToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "k8s" => kubernetes::tools::build_kubernetes_toolset(
            name,
            kubernetes::config::KubernetesToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

/// The hosted-service families ported from the Python-only set in one batch
/// (`aha`, `bigquery`, `bitbucket`, `carrier`, `confluence`, `figma`, `gitlab`,
/// `jira`). They live outside
/// `materialize_a_to_k` only to keep each dispatch function under clippy's
/// line limit; the arms stay at the indentation the elitea-main capability
/// gates read.
fn materialize_ported_saas(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        // No runtime host has an artifact-read grant plane, so Aha is built
        // without one and serves every tool but attach_file.
        "aha" => aha::tools::build_aha_toolset(
            name,
            aha::config::AhaToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            None,
        )
        .map_err(|error| family_error(error.code()))?,
        "bigquery" => bigquery::tools::build_bigquery_toolset(
            name,
            bigquery::config::BigQueryToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "bitbucket" => bitbucket::tools::build_bitbucket_toolset(
            name,
            bitbucket::config::BitbucketToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "carrier" => carrier::tools::build_carrier_toolset(
            name,
            carrier::config::CarrierToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "confluence" => confluence::tools::build_confluence_toolset(
            name,
            confluence::config::ConfluenceToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "figma" => figma::tools::build_figma_toolset(
            name,
            figma::config::FigmaToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "gitlab" => gitlab::tools::build_gitlab_toolset(
            name,
            gitlab::config::GitLabToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "jira" => jira::tools::build_jira_toolset(
            name,
            jira::config::JiraToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

fn materialize_p_to_z(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        "postman" => postman::tools::build_postman_toolset(
            name,
            postman::config::PostmanToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "rally" => rally::tools::build_rally_toolset(
            name,
            rally::config::RallyToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "report_portal" => report_portal::tools::build_report_portal_toolset(
            name,
            report_portal::config::ReportPortalToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "salesforce" => salesforce::tools::build_salesforce_toolset(
            name,
            salesforce::config::SalesforceToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "service_now" => service_now::tools::build_service_now_toolset(
            name,
            service_now::config::ServiceNowToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "slack" => slack::tools::build_slack_toolset(
            name,
            slack::config::SlackToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "sonar" => sonar::tools::build_sonar_read_only_toolset(
            name,
            sonar::config::SonarToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        // Without `toolkit-sql` a `sql` toolkit falls through to unsupported
        // and is skipped like any family this runtime cannot serve.
        #[cfg(feature = "toolkit-sql")]
        "sql" => sql::tools::build_sql_toolset(
            name,
            sql::config::SqlToolkitConfig::parse(settings).map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "yagmail" => yagmail::tools::build_yagmail_toolset(
            name,
            yagmail::config::YagmailToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "zephyr" => zephyr::tools::build_zephyr_toolset(
            name,
            zephyr::config::ZephyrToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "zephyr_squad" => zephyr_squad::tools::build_zephyr_squad_toolset(
            name,
            zephyr_squad::config::ZephyrSquadToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        // MCP and nested applications are rejected above by kind.
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

/// The test-management families. A selection that names only tools a
/// partial family does not serve skips the toolkit, as an unsupported family
/// is skipped.
fn materialize_test_management(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        "qtest" => qtest::tools::build_qtest_toolset(
            name,
            qtest::config::QtestToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "testio" => testio::tools::build_testio_toolset(
            name,
            testio::config::TestIoToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "testrail" => testrail::tools::build_testrail_toolset(
            name,
            testrail::config::TestRailToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "xray_cloud" => xray_cloud::tools::build_xray_cloud_toolset(
            name,
            xray_cloud::config::XrayToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

const fn invalid_configuration() -> ToolsetMaterializationError {
    ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::InvalidConfiguration)
}

const fn unsupported_toolkit() -> ToolsetMaterializationError {
    ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::UnsupportedToolkit)
}

const fn resource_exhausted() -> ToolsetMaterializationError {
    ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::ResourceExhausted)
}

/// The Office families (ADR-0032 P3, P4): `pptx`, `docx` and `xlsx`. Like
/// `artifact`, their authority is the live claim: the Office host rides on the
/// artifact authority. A path that lends none (the desktop until P6) skips the
/// toolkit as unsupported.
#[cfg(feature = "toolkit-office")]
fn materialize_office(
    family: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    use super::families::office_common::{OfficeConfigError, OfficeToolsetError};
    let authority = artifacts.ok_or_else(unsupported_toolkit)?;
    let config_error = |error: OfficeConfigError| office_config_materialization_error(&error);
    let toolset_error = |error: OfficeToolsetError| office_toolset_materialization_error(&error);
    let built = match family {
        "docx" => {
            let config = docx::config::DocxToolkitConfig::parse(settings).map_err(config_error)?;
            let toolset = docx::tools::build_docx_toolset(name, &config, policy, authority)
                .map_err(toolset_error)?;
            toolset.map(|t| Arc::new(t) as Arc<dyn Toolset>)
        }
        "xlsx" => {
            let config = xlsx::config::XlsxToolkitConfig::parse(settings).map_err(config_error)?;
            let toolset = xlsx::tools::build_xlsx_toolset(name, &config, policy, authority)
                .map_err(toolset_error)?;
            toolset.map(|t| Arc::new(t) as Arc<dyn Toolset>)
        }
        "pptx" => {
            let config = pptx::config::PptxToolkitConfig::parse(settings).map_err(config_error)?;
            let toolset = pptx::tools::build_pptx_toolset(name, &config, policy, authority)
                .map_err(toolset_error)?;
            toolset.map(|t| Arc::new(t) as Arc<dyn Toolset>)
        }
        _ => return Err(unsupported_toolkit()),
    };
    built.ok_or_else(unsupported_toolkit)
}

#[cfg(feature = "toolkit-office")]
fn office_config_materialization_error(
    error: &super::families::office_common::OfficeConfigError,
) -> ToolsetMaterializationError {
    use super::families::office_common::OfficeConfigErrorCode;
    match error.code() {
        OfficeConfigErrorCode::InvalidConfiguration => invalid_configuration(),
        OfficeConfigErrorCode::ResourceExhausted => resource_exhausted(),
    }
}

#[cfg(feature = "toolkit-office")]
fn office_toolset_materialization_error(
    error: &super::families::office_common::OfficeToolsetError,
) -> ToolsetMaterializationError {
    use super::families::office_common::OfficeToolsetErrorCode;
    match error.code() {
        OfficeToolsetErrorCode::ResourceExhausted => resource_exhausted(),
        OfficeToolsetErrorCode::InvalidConfiguration
        | OfficeToolsetErrorCode::InvalidDefinition => invalid_configuration(),
    }
}

/// The `imagegen` family (#864). Like `pptx`, its authority is the live claim:
/// the images API and the object API ride on the Office host of the artifact
/// authority. A path that lends no host that reaches the images API (the
/// desktop until P6) skips the toolkit as unsupported.
fn materialize_imagegen(
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    use super::families::imagegen::{self, ImageGenConfigErrorCode};
    let authority = artifacts.ok_or_else(unsupported_toolkit)?;
    let config =
        imagegen::ImageGenToolkitConfig::parse(settings).map_err(|error| match error.code() {
            ImageGenConfigErrorCode::InvalidConfiguration => invalid_configuration(),
            ImageGenConfigErrorCode::ResourceExhausted => resource_exhausted(),
        })?;
    let toolset = imagegen::build_imagegen_toolset(name, &config, policy, authority)
        .map_err(|_| invalid_configuration())?
        .ok_or_else(unsupported_toolkit)?;
    Ok(Arc::new(toolset))
}

const fn artifact_config_materialization_error(
    code: artifact::config::ArtifactConfigErrorCode,
) -> ToolsetMaterializationError {
    match code {
        artifact::config::ArtifactConfigErrorCode::InvalidConfiguration => invalid_configuration(),
        artifact::config::ArtifactConfigErrorCode::ResourceExhausted => resource_exhausted(),
    }
}

const fn artifact_toolset_materialization_error(
    code: artifact::tools::ArtifactToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        artifact::tools::ArtifactToolsetErrorCode::InvalidConfiguration
        | artifact::tools::ArtifactToolsetErrorCode::InvalidDefinition => invalid_configuration(),
        artifact::tools::ArtifactToolsetErrorCode::ResourceExhausted => resource_exhausted(),
    }
}

const fn openapi_materialization_error(
    code: openapi::config::OpenApiConfigErrorCode,
) -> ToolsetMaterializationError {
    match code {
        openapi::config::OpenApiConfigErrorCode::InvalidConfiguration => invalid_configuration(),
        openapi::config::OpenApiConfigErrorCode::ResourceExhausted => resource_exhausted(),
        openapi::config::OpenApiConfigErrorCode::UnsupportedCapability => unsupported_toolkit(),
    }
}

const fn github_toolset_materialization_error(
    code: github::tools::GitHubToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        github::tools::GitHubToolsetErrorCode::InvalidConfiguration
        | github::tools::GitHubToolsetErrorCode::Client
        | github::tools::GitHubToolsetErrorCode::InvalidDefinition => invalid_configuration(),
        github::tools::GitHubToolsetErrorCode::UnsupportedSelection => unsupported_toolkit(),
    }
}

const fn openapi_toolset_materialization_error(
    code: openapi::tools::OpenApiToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        openapi::tools::OpenApiToolsetErrorCode::InvalidConfiguration
        | openapi::tools::OpenApiToolsetErrorCode::Client
        | openapi::tools::OpenApiToolsetErrorCode::InvalidDefinition => invalid_configuration(),
        openapi::tools::OpenApiToolsetErrorCode::ResourceExhausted => resource_exhausted(),
        openapi::tools::OpenApiToolsetErrorCode::UnsupportedCapability => unsupported_toolkit(),
    }
}

const fn sharepoint_materialization_error(
    code: sharepoint::config::SharePointConfigErrorCode,
) -> ToolsetMaterializationError {
    match code {
        sharepoint::config::SharePointConfigErrorCode::InvalidConfiguration => {
            invalid_configuration()
        }
        sharepoint::config::SharePointConfigErrorCode::ResourceExhausted => resource_exhausted(),
        sharepoint::config::SharePointConfigErrorCode::UnsupportedCapability => {
            unsupported_toolkit()
        }
    }
}

const fn sharepoint_toolset_materialization_error(
    code: sharepoint::tools::SharePointToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        sharepoint::tools::SharePointToolsetErrorCode::InvalidConfiguration
        | sharepoint::tools::SharePointToolsetErrorCode::Client
        | sharepoint::tools::SharePointToolsetErrorCode::InvalidDefinition => {
            invalid_configuration()
        }
        sharepoint::tools::SharePointToolsetErrorCode::ResourceExhausted => resource_exhausted(),
        sharepoint::tools::SharePointToolsetErrorCode::UnsupportedCapability
        | sharepoint::tools::SharePointToolsetErrorCode::UnsupportedSelection => {
            unsupported_toolkit()
        }
    }
}
