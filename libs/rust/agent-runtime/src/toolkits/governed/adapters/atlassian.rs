//! Jira and Confluence: `static_secret` (an API token or PAT, with the
//! account name for Basic) and `oauth_delegated` (an Atlassian 3LO access
//! token). Target: the configured site base URL.
//!
//! A static secret calls the configured site URL. An Atlassian 3LO token is
//! valid only at the API gateway, `https://api.atlassian.com/ex/<product>/
//! <cloud id>` (#1464): Main resolves the site's cloud id at consent and
//! sends that base as the `api_base` material key, and the adapter calls it
//! in place of the site URL. The envelope's target is still the site: the
//! base is checked to be the gateway path of THIS product before any call.

use std::sync::Arc;

use adk_core::Toolset;
use elitea_connectors::atlassian::ATLASSIAN_GATEWAY_HOST;
use serde_json::{Map, Value};

use super::{base_url_target, with_credential};
use crate::toolkits::families::https_base_url::{self, BasePath};
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
const OAUTH_KEYS: &[&str] = &["access_token", "api_base"];

/// The longest Atlassian cloud id accepted in a gateway path.
const MAX_CLOUD_ID_BYTES: usize = 64;

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

/// The `api_base` of an OAUTH envelope, when it is the gateway path of
/// `product`: `https://api.atlassian.com/ex/<product>/<cloud id>`, with a
/// cloud id of letters, digits and `-`. Anything else is `None`: the token
/// never goes to a host or a product Main did not resolve.
fn gateway_base(api_base: &str, product: &str) -> Option<String> {
    let url = https_base_url::parse(api_base, BasePath::Prefix).ok()?;
    if url.host_str() != Some(ATLASSIAN_GATEWAY_HOST) || url.port().is_some() {
        return None;
    }
    let mut segments = url.path_segments()?;
    let valid = segments.next() == Some("ex")
        && segments.next() == Some(product)
        && segments.next().is_some_and(|cloud_id| {
            !cloud_id.is_empty()
                && cloud_id.len() <= MAX_CLOUD_ID_BYTES
                && cloud_id
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
        && segments.next().is_none();
    valid.then(|| url.as_str().trim_end_matches('/').to_owned())
}

/// The configuration with the envelope's material as its only credential:
/// a static token with an account name is Basic (`username` + `api_key`), a
/// static token alone or an OAuth access token is Bearer (`token`). An
/// OAuth token also replaces the site URL with its gateway base and marks
/// the toolkit Cloud (3LO exists only for Atlassian Cloud). The tool lane
/// (`build_client`) and the index lane (`index::index_settings`) both use
/// this one function, so the two lanes send a token to the same place.
pub(crate) fn atlassian_settings(
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
            let base = envelope
                .material("api_base")
                .and_then(|base| gateway_base(base, adapter.provider_type()))
                .ok_or_else(missing)?;
            let mut settings = with_credential(
                settings,
                configuration,
                &CREDENTIAL_KEYS,
                &[("token", token), ("base_url", &base)],
            )?;
            settings.insert("cloud".to_owned(), Value::Bool(true));
            Ok(settings)
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

#[cfg(test)]
mod tests {
    //! #1464: an OAuth envelope calls the Atlassian gateway, a static one
    //! the site.

    use std::collections::BTreeMap;

    use serde_json::json;
    use zeroize::Zeroizing;

    use super::*;
    use crate::toolkits::families::confluence::config::ConfluenceToolkitConfig;
    use crate::toolkits::families::jira::config::JiraToolkitConfig;
    use crate::toolkits::governed::EnvelopeKind;

    const CLOUD_ID: &str = "11223344-a1b2-c3d4-e5f6-77889900aabb";

    fn envelope(kind: EnvelopeKind, material: &[(&str, &str)]) -> CredentialEnvelope {
        CredentialEnvelope::new(
            "redemption-1".to_owned(),
            kind,
            material
                .iter()
                .map(|(key, value)| ((*key).to_owned(), Zeroizing::new((*value).to_owned())))
                .collect::<BTreeMap<_, _>>(),
            i64::MAX,
            "https://acme.atlassian.net".to_owned(),
        )
    }

    fn settings(configuration: &str) -> Map<String, Value> {
        json!({ configuration: {
            "base_url": "https://acme.atlassian.net/", "token": "claim-time-secret"}})
        .as_object()
        .cloned()
        .expect("object")
    }

    #[test]
    fn an_oauth_jira_envelope_calls_the_gateway_and_a_static_one_the_site() {
        let base = format!("https://api.atlassian.com/ex/jira/{CLOUD_ID}");
        let oauth = envelope(
            EnvelopeKind::OauthAccess,
            &[("access_token", "oauth-token"), ("api_base", &base)],
        );
        let built = atlassian_settings(
            &JiraAdapter,
            &settings("jira_configuration"),
            "jira_configuration",
            &oauth,
        )
        .expect("oauth settings");
        assert_eq!(built["jira_configuration"]["token"], "oauth-token");
        assert_eq!(built["cloud"], true);
        let config = JiraToolkitConfig::parse(&built).expect("jira config");
        assert_eq!(config.test_base_url().trim_end_matches('/'), base);
        assert!(config.test_cloud());

        let pat = envelope(EnvelopeKind::Static, &[("token", "pat")]);
        let built = atlassian_settings(
            &JiraAdapter,
            &settings("jira_configuration"),
            "jira_configuration",
            &pat,
        )
        .expect("static settings");
        let config = JiraToolkitConfig::parse(&built).expect("jira config");
        assert_eq!(
            config.test_base_url().trim_end_matches('/'),
            "https://acme.atlassian.net"
        );
    }

    #[test]
    fn an_oauth_confluence_envelope_calls_the_gateway_wiki_root() {
        let base = format!("https://api.atlassian.com/ex/confluence/{CLOUD_ID}");
        let oauth = envelope(
            EnvelopeKind::OauthAccess,
            &[("access_token", "oauth-token"), ("api_base", &base)],
        );
        let built = atlassian_settings(
            &ConfluenceAdapter,
            &settings("confluence_configuration"),
            "confluence_configuration",
            &oauth,
        )
        .expect("oauth settings");
        let config = ConfluenceToolkitConfig::parse(&built).expect("confluence config");
        assert_eq!(
            config.test_urls().1.trim_end_matches('/'),
            format!("{base}/wiki")
        );

        let pat = envelope(EnvelopeKind::Static, &[("token", "pat")]);
        let built = atlassian_settings(
            &ConfluenceAdapter,
            &settings("confluence_configuration"),
            "confluence_configuration",
            &pat,
        )
        .expect("static settings");
        let config = ConfluenceToolkitConfig::parse(&built).expect("confluence config");
        assert_eq!(
            config.test_urls().1.trim_end_matches('/'),
            "https://acme.atlassian.net/wiki"
        );
    }

    /// End to end through the governed client: the dial of an OAuth call
    /// goes to the gateway host, the dial of a static call to the site. The
    /// run's allowlist names neither, so each call is refused before any
    /// byte leaves, and the refusal names the host it would have reached.
    #[tokio::test]
    async fn the_governed_call_dials_the_gateway_for_oauth_and_the_site_for_a_pat() {
        use crate::egress::{EGRESS_HOST_NOT_ALLOWED, PublicPolicy, RunEgress};
        use crate::toolkits::governed::adapters::reference_registry;
        use crate::toolkits::governed::client::GovernedToolkitSpec;
        use crate::toolkits::governed::{
            CredentialRedeemer, GovernedOperationClient, RedeemOutcome,
        };
        use crate::toolkits::governed_tests::{RecordingRedeemer, context, tool};
        use crate::toolkits::policy::ToolAdmissionPolicy;

        /// One case: the envelope kind, its material and the host dialled.
        type Case = (EnvelopeKind, Vec<(&'static str, String)>, &'static str);

        let base = format!("https://api.atlassian.com/ex/jira/{CLOUD_ID}");
        let cases: [Case; 2] = [
            (
                EnvelopeKind::OauthAccess,
                vec![
                    ("access_token", "oauth-token".to_owned()),
                    ("api_base", base.clone()),
                ],
                "api.atlassian.com",
            ),
            (
                EnvelopeKind::Static,
                vec![("token", "pat".to_owned())],
                "acme.atlassian.net",
            ),
        ];
        for (kind, material, host) in cases {
            let egress =
                RunEgress::new(PublicPolicy::allowlist(["api.example.com"]).expect("allowlist"));
            let policy = Arc::new(
                ToolAdmissionPolicy::new(&[], &BTreeMap::new())
                    .expect("policy")
                    .with_egress(Arc::clone(&egress)),
            );
            let redeemer = RecordingRedeemer::new(Box::new(move |_| {
                let pairs: Vec<(&str, &str)> = material
                    .iter()
                    .map(|(key, value)| (*key, value.as_str()))
                    .collect();
                Ok(RedeemOutcome::Envelope(envelope(kind, &pairs)))
            }));
            let client = Arc::new(GovernedOperationClient::new(
                Arc::new(reference_registry()),
                Arc::clone(&redeemer) as Arc<dyn CredentialRedeemer>,
            ));
            let toolset = client
                .toolset(&GovernedToolkitSpec::new(
                    "governed",
                    "jira",
                    Some(31),
                    &settings("jira_configuration"),
                    &policy,
                ))
                .expect("governed jira toolset");
            let error = tool(&toolset, "list_projects")
                .await
                .execute(context(), json!({}))
                .await
                .expect_err("the host is off the run's allowlist");
            assert_eq!(error.code, EGRESS_HOST_NOT_ALLOWED, "{error:?}");
            let summary = egress.summary();
            assert_eq!(summary.refused.len(), 1, "{summary:?}");
            assert_eq!(summary.refused[0].host, host, "{kind:?}");
        }
    }

    /// The token never goes anywhere but the gateway path of its own
    /// product: another host, another product, a port, a deeper path or no
    /// base at all refuses the envelope.
    #[test]
    fn an_oauth_envelope_without_its_own_gateway_base_is_refused() {
        let other_product = format!("https://api.atlassian.com/ex/confluence/{CLOUD_ID}");
        let deeper = format!("https://api.atlassian.com/ex/jira/{CLOUD_ID}/rest");
        let port = format!("https://api.atlassian.com:8443/ex/jira/{CLOUD_ID}");
        let host = format!("https://evil.example/ex/jira/{CLOUD_ID}");
        let plain = format!("http://api.atlassian.com/ex/jira/{CLOUD_ID}");
        for base in [
            other_product.as_str(),
            deeper.as_str(),
            port.as_str(),
            host.as_str(),
            plain.as_str(),
            "https://api.atlassian.com/ex/jira/bad_id",
            "https://api.atlassian.com/ex/jira",
        ] {
            let oauth = envelope(
                EnvelopeKind::OauthAccess,
                &[("access_token", "oauth-token"), ("api_base", base)],
            );
            let refused = atlassian_settings(
                &JiraAdapter,
                &settings("jira_configuration"),
                "jira_configuration",
                &oauth,
            );
            assert!(
                matches!(refused, Err(GovernedError::MaterialNotAllowed)),
                "{base}"
            );
        }
        let no_base = envelope(
            EnvelopeKind::OauthAccess,
            &[("access_token", "oauth-token")],
        );
        assert!(matches!(
            atlassian_settings(
                &JiraAdapter,
                &settings("jira_configuration"),
                "jira_configuration",
                &no_base
            ),
            Err(GovernedError::MaterialNotAllowed)
        ));
    }
}
