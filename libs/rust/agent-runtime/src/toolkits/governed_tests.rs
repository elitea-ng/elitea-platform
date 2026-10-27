//! The governed operation client and the five reference adapters (#1120).

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use zeroize::Zeroizing;

use super::delegated_authorization_requirement;
use super::families::index_tools::{IndexToolSupport, IndexToolsWithheld};
use super::families::{aha, confluence, jira, qtest, sharepoint};
use super::governed::adapters::reference_registry;
use super::governed::client::GovernedToolkitSpec;
use super::governed::{
    BoundToolInvocation, CredentialClass, CredentialEnvelope, CredentialRedeemDenial,
    CredentialRedeemer, EnvelopeKind, GovernedError, GovernedOperationClient, RedeemFailure,
    RedeemOutcome, TargetKind,
};
use super::materialize::{
    LeftOutReason, materialize_configured_toolsets_reporting_refusals,
    materialize_configured_toolsets_reporting_refusals_governed,
};
use super::policy::ToolAdmissionPolicy;
use super::snapshot::FrozenToolSnapshot;

pub(super) const FAR_FUTURE: i64 = 4_102_444_800_000;
const CLAIM_TIME_SECRET: &str = "claim-time-secret-never-used";

pub(super) type Respond =
    dyn Fn(&BoundToolInvocation) -> Result<RedeemOutcome, RedeemFailure> + Send + Sync;

/// A redeemer that records every invocation and answers with `respond`.
pub(super) struct RecordingRedeemer {
    pub(super) calls: Mutex<Vec<BoundToolInvocation>>,
    respond: Box<Respond>,
}

impl RecordingRedeemer {
    pub(super) fn new(respond: Box<Respond>) -> Arc<Self> {
        Arc::new(Self {
            calls: Mutex::new(Vec::new()),
            respond,
        })
    }

    pub(super) fn calls(&self) -> Vec<BoundToolInvocation> {
        self.calls.lock().expect("calls").clone()
    }
}

#[async_trait]
impl CredentialRedeemer for RecordingRedeemer {
    async fn redeem(
        &self,
        invocation: BoundToolInvocation,
    ) -> Result<RedeemOutcome, RedeemFailure> {
        let outcome = (self.respond)(&invocation);
        self.calls.lock().expect("calls").push(invocation);
        outcome
    }
}

pub(super) fn envelope(
    kind: EnvelopeKind,
    material: &[(&str, &str)],
    target: &str,
) -> CredentialEnvelope {
    CredentialEnvelope::new(
        "redemption-1".to_owned(),
        kind,
        material
            .iter()
            .map(|(key, value)| ((*key).to_owned(), Zeroizing::new((*value).to_owned())))
            .collect::<BTreeMap<_, _>>(),
        FAR_FUTURE,
        target.to_owned(),
    )
}

pub(super) fn answering(
    kind: EnvelopeKind,
    material: &'static [(&'static str, &'static str)],
    target: &'static str,
) -> Box<Respond> {
    Box::new(move |_| Ok(RedeemOutcome::Envelope(envelope(kind, material, target))))
}

pub(super) fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

pub(super) fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("governed-test").with_function_call_id("call-1"))
}

pub(super) async fn tool(toolset: &Arc<dyn Toolset>, name: &str) -> Arc<dyn Tool> {
    let readonly: Arc<dyn ReadonlyContext> = context();
    toolset
        .tools(readonly)
        .await
        .expect("tools")
        .into_iter()
        .find(|tool| tool.name() == name)
        .unwrap_or_else(|| panic!("{name} is declared"))
}

fn object(value: &Value) -> Map<String, Value> {
    value.as_object().cloned().expect("object")
}

struct Family {
    tool_type: &'static str,
    settings: Map<String, Value>,
    target: &'static str,
    kind: EnvelopeKind,
    material: &'static [(&'static str, &'static str)],
    tool: &'static str,
    arguments: Value,
    operation: &'static str,
    resource: &'static str,
}

fn families() -> Vec<Family> {
    vec![
        Family {
            tool_type: "jira",
            settings: object(&json!({"jira_configuration": {
                "base_url": "https://jira.example.test/jira/", "token": CLAIM_TIME_SECRET}})),
            target: "https://jira.example.test/jira",
            kind: EnvelopeKind::OauthAccess,
            material: &[("access_token", "envelope-jira")],
            tool: "list_comments",
            arguments: json!({"issue_key": "ENG-7", "unexpected": {"bad": true}}),
            operation: "jira.issue.read",
            resource: "jira:issue/ENG-7",
        },
        Family {
            tool_type: "confluence",
            settings: object(&json!({"confluence_configuration": {
                "base_url": "https://wiki.example.test/confluence", "hosting": "Server",
                "username": "someone", "api_key": CLAIM_TIME_SECRET}})),
            target: "https://wiki.example.test/confluence",
            kind: EnvelopeKind::Static,
            material: &[("token", "envelope-wiki"), ("username", "agent")],
            tool: "read_page_by_id",
            arguments: json!({"page_id": [1]}),
            operation: "confluence.page.read",
            // A list is no page id: the touched page is undeterminable.
            resource: "",
        },
        Family {
            tool_type: "aha",
            settings: object(&json!({"aha_configuration": {
                "base_url": "https://tenant.example.test", "api_key": CLAIM_TIME_SECRET}})),
            target: "https://tenant.example.test",
            kind: EnvelopeKind::Static,
            material: &[("token", "envelope-aha")],
            tool: "get_feature",
            arguments: json!({"reference_or_id": 42, "unexpected": true}),
            operation: "aha.feature.read",
            resource: "aha:feature/42",
        },
        Family {
            tool_type: "sharepoint",
            settings: object(&json!({
                "sharepoint_configuration": {
                    "client_id": "client-id",
                    "client_secret": CLAIM_TIME_SECRET,
                    "site_url": "https://tenant.sharepoint.com/sites/default",
                    "oauth_discovery_endpoint": "https://login.microsoftonline.com/tenant"
                },
                "site_path": "teams/project",
                "selected_tools": ["get_lists", "read_list"]
            })),
            target: "https://tenant.sharepoint.com/teams/project",
            kind: EnvelopeKind::OauthAccess,
            material: &[("access_token", "envelope-graph")],
            tool: "read_list",
            arguments: json!({"list_title": 5}),
            operation: "sharepoint.list.read",
            resource: "sharepoint:list/5",
        },
        Family {
            tool_type: "qtest",
            settings: object(&json!({
                "qtest_configuration": {
                    "base_url": "https://qtest.example.test/", "qtest_api_token": CLAIM_TIME_SECRET},
                "qtest_project_id": 7
            })),
            target: "https://qtest.example.test",
            kind: EnvelopeKind::Static,
            material: &[("token", "envelope-qtest")],
            tool: "find_test_case_by_id",
            arguments: json!({"test_id": {"not": "an id"}}),
            operation: "qtest.test_case.read",
            resource: "",
        },
    ]
}

fn governed(respond: Box<Respond>) -> (Arc<GovernedOperationClient>, Arc<RecordingRedeemer>) {
    let redeemer = RecordingRedeemer::new(respond);
    let client = Arc::new(GovernedOperationClient::new(
        Arc::new(reference_registry()),
        Arc::clone(&redeemer) as Arc<dyn CredentialRedeemer>,
    ));
    (client, redeemer)
}

fn toolset(client: &Arc<GovernedOperationClient>, family: &Family) -> Arc<dyn Toolset> {
    client
        .toolset(&GovernedToolkitSpec::new(
            "governed",
            family.tool_type,
            Some(31),
            &family.settings,
            &policy(),
        ))
        .unwrap_or_else(|error| panic!("{} governed toolset: {error}", family.tool_type))
}

/// Each reference family: the call is bound to the catalogue operation,
/// redeemed exactly once, and served by the family's own tool on a client
/// built from the envelope (the call's malformed arguments are the family's
/// own refusal, not a governed one, and nothing reaches the network).
#[tokio::test]
async fn each_reference_family_redeems_once_and_runs_on_the_envelope_client() {
    for family in families() {
        let (kind, material, target) = (family.kind, family.material, family.target);
        let (client, redeemer) = governed(answering(kind, material, target));
        let toolset = toolset(&client, &family);
        let tool = tool(&toolset, family.tool).await;
        let error = tool
            .execute(context(), family.arguments.clone())
            .await
            .expect_err("the family refuses the malformed arguments");
        assert!(
            !error.code.starts_with("governed."),
            "{}: {} {}",
            family.tool_type,
            error.code,
            error.message
        );
        let calls = redeemer.calls();
        assert_eq!(calls.len(), 1, "{}", family.tool_type);
        let call = &calls[0];
        assert_eq!(call.operation_id, family.operation, "{}", family.tool_type);
        assert_eq!(call.resource_ref, family.resource, "{}", family.tool_type);
        assert_eq!(call.toolkit_id, 31);
        assert_eq!(call.tool_call_id, "call-1");
        assert_eq!(call.invocation_id.len(), 36);
        assert_eq!(&call.invocation_id[14..15], "7", "a UUID v7");
        let digest = ring::digest::digest(
            &ring::digest::SHA256,
            &crate::canonical::to_vec(&family.arguments).expect("arguments"),
        );
        assert_eq!(call.arguments_digest.as_slice(), digest.as_ref());
        // No claim-time credential reaches the tool's message.
        assert!(!error.message.contains(CLAIM_TIME_SECRET));
    }
}

#[tokio::test]
async fn an_unmapped_tool_is_denied_without_a_redemption() {
    let family = families().remove(0);
    let (client, redeemer) = governed(answering(family.kind, family.material, family.target));
    let toolset = toolset(&client, &family);
    let generic = tool(&toolset, "execute_generic_rq").await;
    let error = generic
        .execute(
            context(),
            json!({"method": "GET", "relative_url": "/rest/api/2/myself"}),
        )
        .await
        .expect_err("not mapped");
    assert_eq!(error.code, "governed.operation_not_mapped");
    assert_eq!(redeemer.calls().len(), 0);
}

#[tokio::test]
async fn aha_generic_record_tools_map_only_for_features() {
    let adapter = reference_registry();
    let aha = adapter.adapter("aha").expect("aha");
    assert_eq!(
        aha.operation_for(
            "update_record",
            &json!({"record_type": "Features", "record_id": "F-1"})
        )
        .expect("a feature write")
        .operation_id(),
        "aha.feature.write"
    );
    assert!(
        aha.operation_for(
            "update_record",
            &json!({"record_type": "release", "record_id": "R-1"})
        )
        .is_err()
    );
    assert!(aha.operation_for("get_release", &json!({})).is_err());
}

/// Aha!'s generic tools read the kind from ONE field (`record_type`, or
/// `resource_type` for comments); the other one naming a different kind
/// makes the call ambiguous and unmapped.
#[test]
fn aha_reads_the_kind_only_from_the_field_the_family_reads() {
    let registry = reference_registry();
    let aha = registry.adapter("aha").expect("aha");
    for arguments in [
        json!({"record_type": "release", "resource_type": "feature", "record_id": "R-1"}),
        json!({"record_type": "feature", "resource_type": "release", "record_id": "F-1"}),
        json!({"resource_type": "feature", "record_id": "F-1"}),
    ] {
        assert!(
            aha.operation_for("update_record", &arguments).is_err(),
            "{arguments}"
        );
    }
    assert!(
        aha.operation_for(
            "add_comment",
            &json!({"record_type": "feature", "resource_type": "idea", "resource_id": "I-1"})
        )
        .is_err()
    );
    let comment = aha
        .operation_for(
            "add_comment",
            &json!({"resource_type": "feature", "record_type": "features",
                "resource_id": "F-9", "record_id": "F-1"}),
        )
        .expect("a feature comment");
    assert_eq!(comment.resource_ref(), "aha:feature/F-9");
}

fn bound(adapter: &str, tool: &str, arguments: &Value) -> String {
    reference_registry()
        .adapter(adapter)
        .expect("adapter")
        .operation_for(tool, arguments)
        .unwrap_or_else(|_| panic!("{adapter}.{tool} maps"))
        .resource_ref()
        .to_owned()
}

/// Jira `update_issue` acts on the key inside `issue_json`; another key
/// argument naming a different issue is not mapped.
#[test]
fn jira_update_issue_binds_the_key_inside_issue_json() {
    let registry = reference_registry();
    let jira = registry.adapter("jira").expect("jira");
    let bound = |adapter: &str, tool: &str, arguments: Value| bound(adapter, tool, &arguments);
    // Two issues: no single resource.
    assert_eq!(
        bound(
            "jira",
            "link_issues",
            json!({"inward_issue_key": "A-1", "outward_issue_key": "B-2"})
        ),
        ""
    );
    let issue_json = r#"{"key": "ENG-7", "fields": {"summary": "s"}}"#;
    assert_eq!(
        bound("jira", "update_issue", json!({"issue_json": issue_json})),
        "jira:issue/ENG-7"
    );
    assert_eq!(
        bound(
            "jira",
            "update_issue",
            json!({"issue_json": issue_json, "issue_key": "ENG-7"})
        ),
        "jira:issue/ENG-7"
    );
    assert_eq!(
        bound("jira", "update_issue", json!({"issue_json": "not json"})),
        ""
    );
    for other in ["issue_key", "jira_issue_key", "issue_id", "key"] {
        assert!(
            jira.operation_for(
                "update_issue",
                &json!({"issue_json": issue_json, other: "OPS-1"})
            )
            .is_err(),
            "a mismatched {other} is not mapped"
        );
    }
    assert!(
        jira.operation_for(
            "update_issue",
            &json!({"issue_json": "{}", "issue_key": "OPS-1"})
        )
        .is_err()
    );
}

/// Each tool binds only the argument it acts on; an argument the tool
/// ignores never names the resource.
#[test]
fn the_resource_is_bound_only_from_the_argument_the_tool_acts_on() {
    let bound = |adapter: &str, tool: &str, arguments: Value| bound(adapter, tool, &arguments);
    // A JQL search touches whatever the query matches: no resource.
    assert_eq!(
        bound(
            "jira",
            "search_using_jql",
            json!({"jql": "project = ENG", "project_key": "ENG"})
        ),
        ""
    );
    // `list_comments` reads `issue_key`; `jira_issue_key` is not its argument.
    assert_eq!(
        bound(
            "jira",
            "list_comments",
            json!({"jira_issue_key": "OPS-1", "issue_key": "ENG-7"})
        ),
        "jira:issue/ENG-7"
    );
    assert_eq!(
        bound("jira", "list_comments", json!({"jira_issue_key": "OPS-1"})),
        ""
    );
    assert_eq!(
        bound(
            "confluence",
            "site_search",
            json!({"query": "x", "space": "ENG"})
        ),
        ""
    );
    assert_eq!(
        bound(
            "confluence",
            "update_labels",
            json!({"page_ids": ["1"], "id": "1"})
        ),
        ""
    );
    assert_eq!(
        bound(
            "confluence",
            "get_page_tree",
            json!({"id": "9", "page_id": "4"})
        ),
        "confluence:page/4"
    );
    assert_eq!(
        bound("aha", "get_feature", json!({"feature_id": "F-1"})),
        ""
    );
    assert_eq!(
        bound(
            "aha",
            "list_features",
            json!({"product_id": "P-1", "release_id": "R-1"})
        ),
        ""
    );
    assert_eq!(
        bound("aha", "list_features", json!({"product_id": "P-1"})),
        "aha:product/P-1"
    );
    assert_eq!(
        bound(
            "qtest",
            "find_test_case_by_id",
            json!({"test_case_id": "TC-1"})
        ),
        ""
    );
    assert_eq!(
        bound("qtest", "search_by_dql", json!({"dql": "Id = 'TC-1'"})),
        ""
    );
    assert_eq!(
        bound(
            "sharepoint",
            "get_files_list",
            json!({"folder_name": "Shared", "form_name": "Forms"})
        ),
        ""
    );
    assert_eq!(
        bound(
            "sharepoint",
            "read_document",
            json!({"file_path": "/a.docx"})
        ),
        ""
    );
    assert_eq!(
        bound("sharepoint", "read_document", json!({"path": "/a.docx"})),
        "sharepoint:file//a.docx"
    );
}

/// Main binds every grant to a toolkit id: a toolkit without one (or with
/// one Main cannot hold) is not governable and advertises no tools. (The
/// materializer reports this error as `toolkit_not_governable`, as the
/// unadapted-family test shows; the frozen snapshot already refuses a stored
/// toolkit whose id is past `i64::MAX`.)
#[tokio::test]
async fn a_toolkit_without_a_bindable_id_is_not_governable() {
    let family = families().remove(0);
    let (client, redeemer) = governed(answering(family.kind, family.material, family.target));
    for toolkit_id in [None, Some(0), Some(u64::MAX), Some(1 << 63)] {
        let refused = client.toolset(&GovernedToolkitSpec::new(
            "governed",
            family.tool_type,
            toolkit_id,
            &family.settings,
            &policy(),
        ));
        assert!(
            matches!(refused, Err(GovernedError::ToolkitNotGovernable)),
            "{toolkit_id:?}"
        );
    }

    assert_eq!(redeemer.calls().len(), 0);
}

#[tokio::test]
async fn an_envelope_for_another_target_or_an_expired_one_is_refused() {
    let family = families().remove(2);
    let (client, redeemer) = governed(answering(
        family.kind,
        family.material,
        "https://other.example.test",
    ));
    let toolset = toolset(&client, &family);
    let error = tool(&toolset, family.tool)
        .await
        .execute(context(), json!({"feature_id": 1}))
        .await
        .expect_err("another target");
    assert_eq!(error.code, "governed.target_mismatch");
    assert_eq!(redeemer.calls().len(), 1);

    let (client, _) = governed(Box::new(|_| {
        Ok(RedeemOutcome::Envelope(CredentialEnvelope::new(
            String::new(),
            EnvelopeKind::Static,
            BTreeMap::from([("token".to_owned(), Zeroizing::new("expired".to_owned()))]),
            1,
            "https://tenant.example.test".to_owned(),
        )))
    }));
    let error = tool(&toolset_for(&client, &family), family.tool)
        .await
        .execute(context(), json!({"feature_id": 1}))
        .await
        .expect_err("expired");
    assert_eq!(error.code, "governed.envelope_expired");

    // A key the class does not allow, or a class the adapter refuses.
    for (kind, material) in [
        (
            EnvelopeKind::Static,
            &[("token", "t"), ("password", "p")][..],
        ),
        (EnvelopeKind::DynamicLease, &[("token", "t")][..]),
    ] {
        let material: Vec<(String, String)> = material
            .iter()
            .map(|(key, value)| ((*key).to_owned(), (*value).to_owned()))
            .collect();
        let (client, _) = governed(Box::new(move |_| {
            let pairs: Vec<(&str, &str)> = material
                .iter()
                .map(|(k, v)| (k.as_str(), v.as_str()))
                .collect();
            Ok(RedeemOutcome::Envelope(envelope(
                kind,
                &pairs,
                "https://tenant.example.test",
            )))
        }));
        let error = tool(&toolset_for(&client, &family), family.tool)
            .await
            .execute(context(), json!({"feature_id": 1}))
            .await
            .expect_err("refused material");
        assert!(
            matches!(
                error.code,
                "governed.material_not_allowed" | "governed.class_not_supported"
            ),
            "{}",
            error.code
        );
    }
}

fn toolset_for(client: &Arc<GovernedOperationClient>, family: &Family) -> Arc<dyn Toolset> {
    toolset(client, family)
}

#[tokio::test]
async fn authorization_required_raises_the_delegated_authorization_signal() {
    let family = families().remove(3);
    let (client, _) = governed(Box::new(|_| {
        Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
            "AUTHORIZATION_REQUIRED",
            "decision-1".to_owned(),
            "authz-ref-0123".to_owned(),
        )))
    }));
    let error = tool(&toolset(&client, &family), family.tool)
        .await
        .execute(context(), json!({"list_title": "Tasks"}))
        .await
        .expect_err("authorization required");
    let requirement = delegated_authorization_requirement(&error).expect("the delegated signal");
    assert_eq!(
        requirement.authorization_reference(),
        Some("authz-ref-0123")
    );
    assert_eq!(requirement.toolkit_type(), "sharepoint");
    assert_eq!(requirement.server_url(), family.target);

    // Any other code is a tool error carrying the stable code only.
    let (client, _) = governed(Box::new(|_| {
        Ok(RedeemOutcome::Denial(CredentialRedeemDenial::new(
            "CONTEXT_MISMATCH",
            "decision-2".to_owned(),
            "ignored".to_owned(),
        )))
    }));
    let error = tool(&toolset(&client, &family), family.tool)
        .await
        .execute(context(), json!({"list_title": "Tasks"}))
        .await
        .expect_err("denied");
    assert_eq!(error.code, "governed.credential_denied");
    assert_eq!(
        error.message,
        "the credential use was refused: CONTEXT_MISMATCH"
    );
    assert!(delegated_authorization_requirement(&error).is_none());

    // An unknown code reads as DENIED; a transport failure is unavailable.
    assert_eq!(
        CredentialRedeemDenial::new("SURPRISE", String::new(), String::new()).code(),
        "DENIED"
    );
    let (client, _) = governed(Box::new(|_| Err(RedeemFailure::new("transport", true))));
    let error = tool(&toolset(&client, &family), family.tool)
        .await
        .execute(context(), json!({"list_title": "Tasks"}))
        .await
        .expect_err("unavailable");
    assert_eq!(error.code, "governed.redeem_unavailable");
}

#[test]
fn an_envelope_never_formats_its_material() {
    let envelope = envelope(
        EnvelopeKind::Static,
        &[("token", "top-secret-value")],
        "https://a.test",
    );
    let rendered = format!("{envelope:?}");
    assert!(!rendered.contains("top-secret-value"), "{rendered}");
    assert!(rendered.contains("redacted"));
}

/// Every mapped operation exists for its provider in the shared catalogue,
/// with the catalogue's resource kind; every material key is allowed for
/// its class; the target kind and probe agree; every mapped tool is one the
/// family serves.
#[test]
fn every_mapping_is_in_the_shared_operation_catalogue() {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../../testdata/proto/runtime/v1/governed-operations/catalogue.json"
    );
    let catalogue: Value =
        serde_json::from_str(&std::fs::read_to_string(path).expect("catalogue")).expect("JSON");
    let registry = reference_registry();
    let mut seen = Vec::new();
    for provider in catalogue["providers"].as_array().expect("providers") {
        let name = provider["provider_type"].as_str().expect("provider type");
        let adapter = registry
            .adapter(name)
            .unwrap_or_else(|| panic!("{name} adapter"));
        seen.push(name.to_owned());
        let target_kind = match adapter.target_kind() {
            TargetKind::BaseUrl => "base_url",
            TargetKind::TenantSite => "tenant_site",
        };
        assert_eq!(provider["target_kind"], target_kind, "{name}");
        assert_eq!(
            provider["probe_operation"],
            adapter.probe_operation(),
            "{name}"
        );
        let operations: BTreeMap<&str, &str> = provider["operations"]
            .as_array()
            .expect("operations")
            .iter()
            .map(|op| {
                (
                    op["id"].as_str().expect("id"),
                    op["resource_kind"].as_str().expect("kind"),
                )
            })
            .collect();
        assert!(
            operations.contains_key(adapter.probe_operation()),
            "{name} probe"
        );
        for row in adapter.tool_operations() {
            assert_eq!(
                operations.get(row.operation_id),
                Some(&row.resource_kind),
                "{name}.{} -> {}",
                row.tool,
                row.operation_id
            );
        }
        let classes: Vec<&str> = provider["auth_classes"]
            .as_array()
            .expect("classes")
            .iter()
            .map(|class| class.as_str().expect("class"))
            .collect();
        for class in CredentialClass::ALL {
            let keys = adapter.material_keys(class);
            if !classes.contains(&class.as_str()) {
                assert_eq!(keys.len(), 0, "{name} {class:?} is not a catalogue class");
                continue;
            }
            assert!(!keys.is_empty(), "{name} {class:?} has no keys");
            let allowed = provider["material_keys"][class.as_str()]
                .as_array()
                .expect("material keys");
            for key in keys {
                assert!(
                    allowed.iter().any(|value| value == key),
                    "{name} {class:?} {key}"
                );
            }
        }
        let served: Vec<&str> = match name {
            "jira" => jira::tools::tool_catalog(),
            "confluence" => confluence::tools::tool_catalog(),
            "aha" => aha::tools::tool_catalog(),
            "sharepoint" => sharepoint::tools::tool_catalog(),
            "qtest" => qtest::tools::tool_catalog(),
            other => panic!("unexpected provider {other}"),
        }
        .into_iter()
        .map(|(tool, _)| tool)
        .collect();
        for row in adapter.tool_operations() {
            assert!(
                served.contains(&row.tool),
                "{name} does not serve {}",
                row.tool
            );
        }
    }
    let mut registered: Vec<String> = registry.provider_types().map(str::to_owned).collect();
    registered.sort();
    seen.sort();
    assert_eq!(registered, seen);
}

fn snapshot_version() -> Value {
    json!({"tools": [
        {"id": 1, "type": "jira", "toolkit_name": "tracker", "settings": {
            "jira_configuration": {"base_url": "https://jira.example.test", "token": CLAIM_TIME_SECRET}}},
        {"id": 2, "type": "slack", "toolkit_name": "chat", "settings": {
            "slack_configuration": {"slack_token": CLAIM_TIME_SECRET, "channel_id": "C12345678"}}}
    ]})
}

/// A governed pass serves the adapted family through its adapter and
/// REPORTS the unadapted one as not governable; the legacy pass (no
/// governed client) builds both from their settings, as before.
#[tokio::test]
async fn a_governed_pass_refuses_unadapted_toolkits_and_the_legacy_pass_is_unchanged() {
    let version = snapshot_version();
    let policy = policy();
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    let (client, redeemer) = governed(answering(
        EnvelopeKind::OauthAccess,
        &[("access_token", "envelope")],
        "https://jira.example.test",
    ));
    let materialized = materialize_configured_toolsets_reporting_refusals_governed(
        &snapshot,
        &policy,
        &Map::new(),
        None,
        IndexToolSupport::Withheld(IndexToolsWithheld::NoVectorStore),
        Some(&client),
    )
    .await
    .expect("governed pass");
    assert_eq!(materialized.toolsets.len(), 1);
    assert_eq!(materialized.left_out.len(), 1);
    let left_out = &materialized.left_out[0];
    assert_eq!(left_out.toolkit_type(), "slack");
    assert_eq!(left_out.cause(), LeftOutReason::ToolkitNotGovernable);
    assert_eq!(left_out.reason_code(), "toolkit_not_governable");
    assert_eq!(redeemer.calls().len(), 0, "assembly redeems nothing");

    let legacy = materialize_configured_toolsets_reporting_refusals(
        &snapshot,
        &policy,
        &Map::new(),
        None,
        IndexToolSupport::Withheld(IndexToolsWithheld::NoVectorStore),
    )
    .await
    .expect("legacy pass");
    assert_eq!(legacy.toolsets.len(), 2);
    assert_eq!(legacy.left_out.len(), 0);
}

/// The worker's canonical target equals Main's for every shared vector: the
/// envelope's `target_identity` is compared by exact string.
#[test]
fn canonical_target_matches_the_shared_vectors() {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../../testdata/proto/runtime/v1/governed-operations/targets.json"
    );
    let vectors: Value =
        serde_json::from_str(&std::fs::read_to_string(path).expect("targets")).expect("JSON");
    for vector in vectors["vectors"].as_array().expect("vectors") {
        let input = vector["input"].as_str().expect("input");
        let url = reqwest::Url::parse(input).expect("vector URL");
        assert_eq!(
            super::governed::canonical_target(&url),
            vector["canonical"].as_str().expect("canonical"),
            "{input}"
        );
    }
}
