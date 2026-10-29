//! The run's public egress policy (#1490) as a toolkit family and `web_fetch`
//! see it: a refused host fails the call as `egress_host_not_allowed` naming
//! the host and port, nothing is dialled, and the run's summary counts it.

use std::sync::Arc;
use std::sync::atomic::Ordering;

use adk_core::{ReadonlyContext, ToolContext};
use adk_tool::SimpleToolContext;
use serde_json::{Map, Value, json};

use super::materialize::materialize_configured_toolsets_with_tokens_and_authorization;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::FrozenToolSnapshot;
use super::web_test_support::{
    FakeMeter, FixtureTransport, call, fixture, origin, web_toolset_under,
};
use crate::egress::{EGRESS_HOST_NOT_ALLOWED, PublicPolicy, RunEgress};

fn policy_under(allow: &[&str]) -> (Arc<ToolAdmissionPolicy>, Arc<RunEgress>) {
    let egress = RunEgress::new(PublicPolicy::allowlist(allow.iter().copied()).expect("policy"));
    let policy = ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new())
        .expect("policy")
        .with_egress(Arc::clone(&egress));
    (Arc::new(policy), egress)
}

fn jira_settings() -> Map<String, Value> {
    json!({
        "jira_configuration": {
            "base_url": "https://jira.example.test/jira",
            "hosting": "Server",
            "username": "svc-elitea",
            "api_key": "jira-private-api-key"
        },
        "limit": 5
    })
    .as_object()
    .cloned()
    .expect("Jira settings")
}

#[tokio::test]
async fn a_toolkit_family_refuses_a_host_outside_the_allowlist() {
    let (policy, egress) = policy_under(&["api.example.com", "*.atlassian.net"]);
    let version = json!({"tools": [
        {"id": 1, "type": "jira", "toolkit_name": "tracker", "settings": jira_settings()}
    ]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy.as_ref());
    // The production path: materialization binds the family's client to the
    // run's egress policy.
    let (toolsets, _) = materialize_configured_toolsets_with_tokens_and_authorization(
        &snapshot,
        &policy,
        &Map::new(),
    )
    .await
    .expect("materialized");
    let context: Arc<dyn ToolContext> =
        Arc::new(SimpleToolContext::new("egress-test").with_function_call_id("call-1"));
    let readonly: Arc<dyn ReadonlyContext> = context.clone();
    let tools = toolsets[0].tools(readonly).await.expect("tools");
    let search = tools
        .iter()
        .find(|tool| tool.name() == "search_using_jql")
        .expect("search tool");
    let error = search
        .execute(context, json!({"jql": "project = OPS"}))
        .await
        .expect_err("refused");
    assert_eq!(error.code, EGRESS_HOST_NOT_ALLOWED, "{error:?}");
    assert!(
        error.message.contains("jira.example.test:443"),
        "{}",
        error.message
    );
    assert!(!error.message.contains("/jira"), "the host, never the URL");
    let summary = egress.summary();
    assert_eq!(summary.refused.len(), 1);
    assert_eq!(summary.refused[0].host, "jira.example.test");
    assert_eq!(summary.reached, []);
}

#[tokio::test]
async fn web_fetch_refuses_a_host_outside_the_allowlist_on_every_hop() {
    let (port, served) = origin().await;
    let meter = FakeMeter::new();
    let tavily = FixtureTransport::answering(&[fixture("tavily", "web_search_ok")]);
    let (policy, egress) = policy_under(&["docs.example.com"]);
    let web = web_toolset_under(
        &json!({"allow_http": true}),
        Some(&meter),
        &tavily,
        vec![port],
        &policy,
    );
    let fetch = |host: &str, path: &str| json!({"url": format!("http://{host}:{port}{path}")});

    let refused = call(&web, "web_fetch", fetch("other.example.com", "/page")).await;
    assert_eq!(
        refused["error"]["code"], EGRESS_HOST_NOT_ALLOWED,
        "{refused}"
    );
    let message = refused["error"]["message"].as_str().expect("message");
    assert!(
        message.contains(&format!("other.example.com:{port}")),
        "{message}"
    );
    assert_eq!(served.load(Ordering::SeqCst), 0, "nothing was dialled");

    let allowed = call(&web, "web_fetch", fetch("docs.example.com", "/page")).await;
    assert_eq!(allowed["backend"], "direct", "{allowed}");

    // A redirect to a host outside the allowlist is refused at the hop, named.
    let hop = call(
        &web,
        "web_fetch",
        fetch("docs.example.com", "/redirect-metadata-name"),
    )
    .await;
    assert_eq!(hop["error"]["code"], EGRESS_HOST_NOT_ALLOWED, "{hop}");
    let message = hop["error"]["message"].as_str().expect("message");
    assert!(
        message.contains(&format!("metadata.example.com:{port}")),
        "{message}"
    );

    let summary = egress.summary();
    let reached: Vec<_> = summary
        .reached
        .iter()
        .map(|host| (host.host.as_str(), host.count))
        .collect();
    assert_eq!(reached, [("docs.example.com", 2)]);
    let refused: Vec<_> = summary
        .refused
        .iter()
        .map(|host| host.host.as_str())
        .collect();
    assert_eq!(refused, ["metadata.example.com", "other.example.com"]);
}
