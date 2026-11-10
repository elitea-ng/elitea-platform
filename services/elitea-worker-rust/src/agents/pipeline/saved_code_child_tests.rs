use std::collections::BTreeSet;
use std::sync::Arc;

use super::*;

fn owner() -> Arc<SavedCodeOwner> {
    Arc::new(SavedCodeOwner::new(
        23,
        29,
        &[0xab; 32],
        BTreeSet::from(["validate".to_owned(), "normalize".to_owned()]),
    ))
}

async fn scope(
    provider: &SavedCodeChildScopeProvider,
    thread: &str,
    node: &str,
) -> Result<serde_json::Value, HttpActionError> {
    provider
        .for_visit(thread, node, SavedChildPurpose::CodeRecovery)
        .await
        .map(|scope| serde_json::to_value(scope).unwrap())
}

/// W3-2b: a root-level Agent node's use names its saved pipeline, Main's
/// frozen digest, the Agent node, the root parent and its own subgraph thread.
#[tokio::test]
async fn a_root_use_names_its_saved_pipeline_and_its_own_thread() {
    let provider = SavedCodeChildScopeProvider::new(owner(), "run_saved", None);
    let value = scope(&provider, "root-thread/run_saved", "validate")
        .await
        .unwrap();
    assert_eq!(
        value,
        serde_json::json!({
            "schema": SAVED_CODE_CHILD_SCOPE_SCHEMA,
            "application_id": 23,
            "version_id": 29,
            "definition_sha256": "ab".repeat(32),
            "agent_node_id": "run_saved",
            "fanout": null,
            "parent_thread_id": "root-thread",
            "child_thread_id": "root-thread/run_saved",
            "code_node_id": "validate",
        })
    );
}

/// Depth 2: a fan-out member's use names the member's family.
#[tokio::test]
async fn a_member_use_names_its_fanout_family() {
    let member = SavedCodeFanoutMember::for_tests("map_item", "each", 2, 4, "m1:item-2");
    let provider = SavedCodeChildScopeProvider::new(owner(), "map_saved", Some(Arc::new(member)));
    let value = scope(&provider, "m1:item-2/map_saved", "normalize")
        .await
        .unwrap();
    assert_eq!(
        value["fanout"],
        serde_json::json!({"kind": "map_item", "node_id": "each", "ordinal": 2, "item_count": 4})
    );
    assert_eq!(value["parent_thread_id"], "m1:item-2");
    assert_eq!(value["code_node_id"], "normalize");
}

/// The provider never serves a visit outside its own use: another node, a
/// node that is not Code, another Agent node's thread, a deeper thread, the
/// parent itself, another member, or another purpose.
#[tokio::test]
async fn a_visit_outside_its_own_use_is_never_served() {
    let root = SavedCodeChildScopeProvider::new(owner(), "run_saved", None);
    for (thread, node) in [
        ("root-thread/run_saved", "route_quality"),
        ("root-thread/run_again", "validate"),
        ("root-thread/run_saved/deeper", "validate"),
        ("root-thread", "validate"),
        ("/run_saved", "validate"),
        ("root-thread/xrun_saved", "validate"),
    ] {
        assert!(
            matches!(
                scope(&root, thread, node).await,
                Err(HttpActionError::PolicyDenied)
            ),
            "{thread} {node}"
        );
    }
    let member = SavedCodeFanoutMember::for_tests("map_item", "each", 1, 4, "m1:item-1");
    let member = SavedCodeChildScopeProvider::new(owner(), "map_saved", Some(Arc::new(member)));
    assert!(matches!(
        scope(&member, "m1:item-2/map_saved", "validate").await,
        Err(HttpActionError::PolicyDenied)
    ));
    for purpose in [
        SavedChildPurpose::NestedHttp,
        SavedChildPurpose::CodeDebug,
        SavedChildPurpose::CodeWorkspace,
        SavedChildPurpose::PlatformBroker,
    ] {
        assert!(matches!(
            root.for_visit("root-thread/run_saved", "validate", purpose)
                .await,
            Err(HttpActionError::PolicyDenied)
        ));
    }
}
