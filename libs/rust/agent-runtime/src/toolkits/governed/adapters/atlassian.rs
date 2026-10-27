//! Jira and Confluence: `static_secret` (an API token or PAT, with the
//! account name for Basic) and `oauth_delegated` (an Atlassian 3LO access
//! token). Target: the configured site base URL.

use std::sync::Arc;

use adk_core::Toolset;
use serde_json::{Map, Value};

use super::{base_url_target, with_credential};
use crate::toolkits::families::https_base_url::BasePath;
use crate::toolkits::governed::{
    AuthAdapter, CredentialClass, CredentialEnvelope, GovernedBuild, GovernedError, TargetKind,
    ToolOperation, admit_material,
};
use crate::toolkits::materialize::governed_family_toolset;

/// The keys an Atlassian configuration may carry a credential under; all are
/// removed before the envelope's material is written.
const CREDENTIAL_KEYS: [&str; 4] = ["token", "api_key", "username", "password"];
const STATIC_KEYS: &[&str] = &["token", "username"];
const OAUTH_KEYS: &[&str] = &["access_token"];

const ISSUE: &[&str] = &["issue_key", "jira_issue_key", "issue_id", "id"];
const PROJECT: &[&str] = &["project_key", "project"];

const JIRA_TOOLS: &[ToolOperation] = &[
    row("list_projects", "jira.project.read", "project", &[]),
    row("search_using_jql", "jira.issue.search", "project", PROJECT),
    row("list_comments", "jira.issue.read", "issue", ISSUE),
    row("get_specific_field_info", "jira.issue.read", "issue", ISSUE),
    row("get_remote_links", "jira.issue.read", "issue", ISSUE),
    row(
        "get_field_with_image_descriptions",
        "jira.issue.read",
        "issue",
        ISSUE,
    ),
    row(
        "get_comments_with_image_descriptions",
        "jira.issue.read",
        "issue",
        ISSUE,
    ),
    row("get_attachments_content", "jira.issue.read", "issue", ISSUE),
    row("create_issue", "jira.issue.write", "issue", &[]),
    row("update_issue", "jira.issue.write", "issue", ISSUE),
    row("modify_labels", "jira.issue.write", "issue", ISSUE),
    row("add_comments", "jira.issue.write", "issue", ISSUE),
    row("set_issue_status", "jira.issue.write", "issue", ISSUE),
    row("link_issues", "jira.issue.write", "issue", ISSUE),
    row(
        "add_file_to_issue_description",
        "jira.issue.write",
        "issue",
        ISSUE,
    ),
    row(
        "update_comment_with_file",
        "jira.issue.write",
        "issue",
        ISSUE,
    ),
];

const PAGE: &[&str] = &["page_id", "id"];
const SPACE: &[&str] = &["space", "space_key"];

const CONFLUENCE_TOOLS: &[ToolOperation] = &[
    row("read_page_by_id", "confluence.page.read", "page", PAGE),
    row("get_page_tree", "confluence.page.read", "page", PAGE),
    row(
        "get_page_with_image_descriptions",
        "confluence.page.read",
        "page",
        PAGE,
    ),
    row("get_page_attachments", "confluence.page.read", "page", PAGE),
    row("search_pages", "confluence.page.search", "space", SPACE),
    row("search_by_title", "confluence.page.search", "space", SPACE),
    row("site_search", "confluence.page.search", "space", SPACE),
    row(
        "get_page_id_by_title",
        "confluence.page.search",
        "space",
        SPACE,
    ),
    row(
        "get_pages_with_label",
        "confluence.page.search",
        "space",
        SPACE,
    ),
    row(
        "list_pages_with_label",
        "confluence.page.search",
        "space",
        SPACE,
    ),
    row("create_page", "confluence.page.write", "page", &[]),
    row("create_pages", "confluence.page.write", "page", &[]),
    row("delete_page", "confluence.page.write", "page", PAGE),
    row("update_page_by_id", "confluence.page.write", "page", PAGE),
    row("update_page_by_title", "confluence.page.write", "page", &[]),
    row("update_pages", "confluence.page.write", "page", &[]),
    row("update_labels", "confluence.page.write", "page", PAGE),
    row("add_file_to_page", "confluence.page.write", "page", PAGE),
];

pub(super) const fn row(
    tool: &'static str,
    operation_id: &'static str,
    resource_kind: &'static str,
    id_arguments: &'static [&'static str],
) -> ToolOperation {
    ToolOperation {
        tool,
        operation_id,
        resource_kind,
        id_arguments,
    }
}

/// The configuration with the envelope's material as its only credential:
/// a static token with an account name is Basic (`username` + `api_key`), a
/// static token alone or an OAuth access token is Bearer (`token`).
fn atlassian_settings(
    adapter: &dyn AuthAdapter,
    settings: &Map<String, Value>,
    configuration: &str,
    envelope: &CredentialEnvelope,
) -> Result<Map<String, Value>, GovernedError> {
    let class = admit_material(adapter, envelope)?;
    let missing = || GovernedError::MaterialNotAllowed;
    match class {
        CredentialClass::StaticSecret => {
            let token = envelope.material("token").ok_or_else(missing)?;
            match envelope.material("username") {
                Some(username) => with_credential(
                    settings,
                    configuration,
                    &CREDENTIAL_KEYS,
                    &[("username", username), ("api_key", token)],
                ),
                None => with_credential(
                    settings,
                    configuration,
                    &CREDENTIAL_KEYS,
                    &[("token", token)],
                ),
            }
        }
        CredentialClass::OauthDelegated => {
            let token = envelope.material("access_token").ok_or_else(missing)?;
            with_credential(
                settings,
                configuration,
                &CREDENTIAL_KEYS,
                &[("token", token)],
            )
        }
    }
}

const fn atlassian_keys(class: CredentialClass) -> &'static [&'static str] {
    match class {
        CredentialClass::StaticSecret => STATIC_KEYS,
        CredentialClass::OauthDelegated => OAUTH_KEYS,
    }
}

/// The Jira adapter.
pub struct JiraAdapter;

impl AuthAdapter for JiraAdapter {
    fn provider_type(&self) -> &'static str {
        "jira"
    }

    fn target_kind(&self) -> TargetKind {
        TargetKind::BaseUrl
    }

    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str] {
        atlassian_keys(class)
    }

    fn probe_operation(&self) -> &'static str {
        "jira.project.read"
    }

    fn tool_operations(&self) -> &'static [ToolOperation] {
        JIRA_TOOLS
    }

    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError> {
        base_url_target(settings, "jira_configuration", BasePath::Prefix)
    }

    fn build_client(
        &self,
        build: &GovernedBuild<'_>,
        envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        let settings = atlassian_settings(self, build.settings(), "jira_configuration", envelope)?;
        governed_family_toolset("jira", build, &settings, &Map::new())
    }
}

/// The Confluence adapter.
pub struct ConfluenceAdapter;

impl AuthAdapter for ConfluenceAdapter {
    fn provider_type(&self) -> &'static str {
        "confluence"
    }

    fn target_kind(&self) -> TargetKind {
        TargetKind::BaseUrl
    }

    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str] {
        atlassian_keys(class)
    }

    fn probe_operation(&self) -> &'static str {
        "confluence.space.read"
    }

    fn tool_operations(&self) -> &'static [ToolOperation] {
        CONFLUENCE_TOOLS
    }

    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError> {
        base_url_target(settings, "confluence_configuration", BasePath::Prefix)
    }

    fn build_client(
        &self,
        build: &GovernedBuild<'_>,
        envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        let settings =
            atlassian_settings(self, build.settings(), "confluence_configuration", envelope)?;
        governed_family_toolset("confluence", build, &settings, &Map::new())
    }
}
