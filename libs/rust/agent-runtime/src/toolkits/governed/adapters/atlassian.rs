//! Jira and Confluence: `static_secret` (an API token or PAT, with the
//! account name for Basic) and `oauth_delegated` (an Atlassian 3LO access
//! token). Target: the configured site base URL.

use std::sync::Arc;

use adk_core::Toolset;
use serde_json::{Map, Value};

use super::{base_url_target, with_credential};
use crate::toolkits::families::https_base_url::BasePath;
use crate::toolkits::governed::{
    AuthAdapter, BoundOperation, CredentialClass, CredentialEnvelope, GovernedBuild, GovernedError,
    OperationNotMapped, TargetKind, ToolOperation, admit_material, resource_identifier,
    table_operation, table_row,
};
use crate::toolkits::materialize::governed_family_toolset;

/// The keys an Atlassian configuration may carry a credential under; all are
/// removed before the envelope's material is written.
const CREDENTIAL_KEYS: [&str; 4] = ["token", "api_key", "username", "password"];
const STATIC_KEYS: &[&str] = &["token", "username"];
const OAUTH_KEYS: &[&str] = &["access_token"];

// Each row binds the ONE argument the family reads for the resource it
// touches; `None` where that resource cannot be told from the arguments
// (a JQL/CQL or text query, a create, two issues, a list of pages).
const JIRA_TOOLS: &[ToolOperation] = &[
    row("list_projects", "jira.project.read", "project", None),
    row("search_using_jql", "jira.issue.search", "project", None),
    row(
        "list_comments",
        "jira.issue.read",
        "issue",
        Some("issue_key"),
    ),
    row(
        "get_specific_field_info",
        "jira.issue.read",
        "issue",
        Some("jira_issue_key"),
    ),
    row(
        "get_remote_links",
        "jira.issue.read",
        "issue",
        Some("jira_issue_key"),
    ),
    row(
        "get_field_with_image_descriptions",
        "jira.issue.read",
        "issue",
        Some("jira_issue_key"),
    ),
    row(
        "get_comments_with_image_descriptions",
        "jira.issue.read",
        "issue",
        Some("jira_issue_key"),
    ),
    row(
        "get_attachments_content",
        "jira.issue.read",
        "issue",
        Some("jira_issue_key"),
    ),
    row("create_issue", "jira.issue.write", "issue", None),
    // The key is inside `issue_json` (`JiraAdapter::operation_for`).
    row("update_issue", "jira.issue.write", "issue", None),
    row(
        "modify_labels",
        "jira.issue.write",
        "issue",
        Some("issue_key"),
    ),
    row(
        "add_comments",
        "jira.issue.write",
        "issue",
        Some("issue_key"),
    ),
    row(
        "set_issue_status",
        "jira.issue.write",
        "issue",
        Some("issue_key"),
    ),
    row("link_issues", "jira.issue.write", "issue", None),
    row(
        "add_file_to_issue_description",
        "jira.issue.write",
        "issue",
        Some("issue_key"),
    ),
    row(
        "update_comment_with_file",
        "jira.issue.write",
        "issue",
        Some("issue_key"),
    ),
];

/// The other issue-key arguments an `update_issue` call may carry; any that
/// names a different issue than `issue_json` does is refused.
const JIRA_KEY_ARGUMENTS: [&str; 4] = ["issue_key", "jira_issue_key", "issue_id", "key"];

const CONFLUENCE_TOOLS: &[ToolOperation] = &[
    row(
        "read_page_by_id",
        "confluence.page.read",
        "page",
        Some("page_id"),
    ),
    row(
        "get_page_tree",
        "confluence.page.read",
        "page",
        Some("page_id"),
    ),
    row(
        "get_page_with_image_descriptions",
        "confluence.page.read",
        "page",
        Some("page_id"),
    ),
    row(
        "get_page_attachments",
        "confluence.page.read",
        "page",
        Some("page_id"),
    ),
    row("search_pages", "confluence.page.search", "space", None),
    row("search_by_title", "confluence.page.search", "space", None),
    row("site_search", "confluence.page.search", "space", None),
    row(
        "get_page_id_by_title",
        "confluence.page.search",
        "space",
        None,
    ),
    row(
        "get_pages_with_label",
        "confluence.page.search",
        "space",
        None,
    ),
    row(
        "list_pages_with_label",
        "confluence.page.search",
        "space",
        None,
    ),
    row("create_page", "confluence.page.write", "page", None),
    row("create_pages", "confluence.page.write", "page", None),
    // The family deletes `page_id` when given, else looks the title up.
    row(
        "delete_page",
        "confluence.page.write",
        "page",
        Some("page_id"),
    ),
    row(
        "update_page_by_id",
        "confluence.page.write",
        "page",
        Some("page_id"),
    ),
    row(
        "update_page_by_title",
        "confluence.page.write",
        "page",
        None,
    ),
    row("update_pages", "confluence.page.write", "page", None),
    row("update_labels", "confluence.page.write", "page", None),
    row(
        "add_file_to_page",
        "confluence.page.write",
        "page",
        Some("page_id"),
    ),
];

pub(super) const fn row(
    tool: &'static str,
    operation_id: &'static str,
    resource_kind: &'static str,
    id_argument: Option<&'static str>,
) -> ToolOperation {
    ToolOperation::new(tool, operation_id, resource_kind, id_argument)
}

/// `update_issue` acts on the issue its `issue_json` names under `key`. A
/// call whose other key arguments name a different issue is not mapped; a
/// call whose `issue_json` names no key binds an empty resource.
fn update_issue_operation(arguments: &Value) -> Result<BoundOperation, OperationNotMapped> {
    let row = table_row(JIRA_TOOLS, "update_issue")?;
    let key = arguments
        .get("issue_json")
        .and_then(Value::as_str)
        .and_then(|text| serde_json::from_str::<Value>(text).ok())
        .and_then(|issue| issue.get("key").and_then(resource_identifier));
    for name in JIRA_KEY_ARGUMENTS {
        let Some(other) = arguments.get(name).filter(|value| !value.is_null()) else {
            continue;
        };
        if resource_identifier(other).is_none() || resource_identifier(other) != key {
            return Err(OperationNotMapped);
        }
    }
    let resource = key
        .map(|key| row.resource("jira", &key))
        .unwrap_or_default();
    BoundOperation::new(row.operation_id, resource, arguments)
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

    fn operation_for(
        &self,
        tool_name: &str,
        arguments: &Value,
    ) -> Result<BoundOperation, OperationNotMapped> {
        if tool_name == "update_issue" {
            return update_issue_operation(arguments);
        }
        table_operation(self.provider_type(), JIRA_TOOLS, tool_name, arguments)
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
