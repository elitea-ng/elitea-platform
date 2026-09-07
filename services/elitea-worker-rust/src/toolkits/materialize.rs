//! Claim-materialized configured toolsets for a direct ADK `LlmAgent`.
//!
//! Main freezes toolkit identity and redeems schema-declared secrets before the
//! worker receives the request. This boundary consumes only that immutable
//! request snapshot, applies one deployment-policy generation, and constructs
//! native ADK toolsets. MCP, nested applications and artifact-owning families
//! have separate authority owners and therefore never fall through this map.

use std::fmt;
use std::sync::Arc;

use adk_rust::Toolset;

use super::DelegatedAuthorizationCatalog;
use super::families::{
    azure, azure_search, elastic, gcp, github, gitlab_org, google_places, keycloak, kubernetes,
    openapi, postman, rally, report_portal, salesforce, service_now, sharepoint, slack, sonar, sql,
    yagmail, zephyr, zephyr_squad,
};
use super::policy::ToolAdmissionPolicy;
use super::snapshot::{AdmittedToolSnapshot, FrozenToolKind, FrozenToolReference};

const MAX_AGENT_TOOLSETS: usize = 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ToolsetMaterializationErrorCode {
    InvalidConfiguration,
    UnsupportedToolkit,
    ResourceExhausted,
}

#[derive(Clone, Copy)]
pub(crate) struct ToolsetMaterializationError {
    code: ToolsetMaterializationErrorCode,
}

impl ToolsetMaterializationError {
    #[must_use]
    pub(crate) const fn code(self) -> ToolsetMaterializationErrorCode {
        self.code
    }
}

impl fmt::Debug for ToolsetMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ToolsetMaterializationError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ToolsetMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
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

pub(crate) fn materialize_configured_toolsets_with_tokens_and_authorization(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    if snapshot.len() > MAX_AGENT_TOOLSETS {
        return Err(resource_exhausted());
    }
    let mut toolsets = Vec::new();
    let mut delegated_authorization = DelegatedAuthorizationCatalog::default();
    for reference in snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Configured)
    {
        let (toolset, authorization) = match materialize(reference, policy, delegated_tokens) {
            Ok(materialized) => materialized,
            Err(error) if error.code() == ToolsetMaterializationErrorCode::UnsupportedToolkit => {
                tracing::warn!(
                    event = "agent_toolkit_skipped",
                    reason_code = "unsupported_toolkit_family",
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "agent toolkit is unavailable in this runtime and was omitted from the native toolsets"
                );
                continue;
            }
            Err(error) => return Err(error),
        };
        delegated_authorization
            .merge(authorization)
            .map_err(|()| invalid_configuration())?;
        toolsets.push(toolset);
    }
    Ok((toolsets, delegated_authorization))
}

fn materialize(
    reference: &FrozenToolReference<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
) -> Result<(Arc<dyn Toolset>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    if reference.kind() != FrozenToolKind::Configured {
        return Err(unsupported_toolkit());
    }
    let settings = reference.settings().ok_or_else(invalid_configuration)?;
    let name = reference.toolkit_name();
    if matches!(
        reference.tool_type(),
        "azure"
            | "azure_search"
            | "elastic"
            | "gcp"
            | "github"
            | "gitlab_org"
            | "google_places"
            | "keycloak"
            | "k8s"
    ) {
        let toolset = materialize_a_to_k(reference.tool_type(), name, settings, policy)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type() == "openapi" {
        let config = openapi::config::OpenApiToolkitConfig::parse(name, settings, delegated_tokens)
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
    let toolset = materialize_p_to_z(reference.tool_type(), name, settings, policy)?;
    Ok((toolset, DelegatedAuthorizationCatalog::default()))
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
        // Aha needs a sealed artifact resolver. MCP and nested applications
        // are rejected above by kind.
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

const fn invalid_configuration() -> ToolsetMaterializationError {
    ToolsetMaterializationError {
        code: ToolsetMaterializationErrorCode::InvalidConfiguration,
    }
}

const fn unsupported_toolkit() -> ToolsetMaterializationError {
    ToolsetMaterializationError {
        code: ToolsetMaterializationErrorCode::UnsupportedToolkit,
    }
}

const fn resource_exhausted() -> ToolsetMaterializationError {
    ToolsetMaterializationError {
        code: ToolsetMaterializationErrorCode::ResourceExhausted,
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
