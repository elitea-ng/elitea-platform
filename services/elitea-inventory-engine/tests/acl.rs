//! Document ACLs (ADR-0028 D3): what a caller may read, through the stored
//! graph's view and the read tools. Needs `INVENTORY_TEST_DSN`.

mod common;

use elitea_content_source::{Acl, Caller, Principal, PrincipalKind};
use elitea_inventory_engine::graph::{Citation, Graph};
use elitea_inventory_engine::retrieval::{Call, ViewCache, dispatch};
use elitea_inventory_engine::store::GraphKey;
use elitea_inventory_engine::store::sources::{
    self, Completion, DocumentState, RunCounts, SourceStatus,
};
use serde_json::{Map, Value, json};
use std::collections::BTreeMap;

fn cite(path: &str) -> Citation {
    Citation {
        file_path: path.to_owned(),
        source_toolkit: Some("wiki".to_owned()),
        doc_id: Some(format!("wiki://{path}")),
        ..Citation::default()
    }
}

fn caller(user: Option<&str>) -> Caller {
    Caller {
        user_id: user.map(str::to_owned),
        ..Caller::default()
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)]
async fn a_restricted_document_is_read_only_by_its_principals() {
    let Some(pool) = common::database("acl").await else {
        return;
    };
    let key = GraphKey::new(3, 30).expect("key");
    let mut graph = Graph::new();
    graph.add_entity(
        "a",
        "Salary Bands",
        "concept",
        Some(&cite("hr/secret.md")),
        None,
    );
    graph.add_entity(
        "b",
        "Leave Policy",
        "concept",
        Some(&cite("hr/public.md")),
        None,
    );
    graph.add_entity(
        "b",
        "Leave Policy",
        "concept",
        Some(&cite("hr/secret.md")),
        None,
    );
    graph.add_entity(
        "c",
        "Onboarding",
        "process",
        Some(&cite("hr/public.md")),
        None,
    );
    let found_in = |path: &str| json!({"discovered_in_file": path, "source_toolkit": "wiki"});
    graph.add_relation("a", "c", "related_to", found_in("hr/secret.md").as_object());
    graph.add_relation("b", "c", "related_to", found_in("hr/public.md").as_object());
    graph.set_community_data(json!({"communities": {"community_0": {
        "members": ["a", "b", "c"], "centroids": [{"id": "a", "score": 1.0}],
        "label": "Compensation", "summary": "Salary bands range from …"}}}));

    sources::start(
        &pool,
        key,
        &SourceStatus {
            toolkit_id: "9".to_owned(),
            toolkit_name: "wiki".to_owned(),
            toolkit_type: "confluence".to_owned(),
            branch: None,
        },
    )
    .await
    .expect("status");
    let restricted = Acl::Restricted {
        principals: vec![Principal {
            kind: PrincipalKind::User,
            id: "7".to_owned(),
        }],
    };
    let documents: BTreeMap<String, DocumentState> =
        [("hr/public.md", Acl::Project), ("hr/secret.md", restricted)]
            .into_iter()
            .map(|(path, acl)| {
                (
                    path.to_owned(),
                    DocumentState {
                        version: "v1".to_owned(),
                        mime: "text/markdown".to_owned(),
                        acl,
                    },
                )
            })
            .collect();
    sources::complete(
        &pool,
        key,
        &graph,
        &Completion {
            toolkit_id: "9",
            source_name: "wiki",
            documents: &documents,
            counts: RunCounts::default(),
            commit_sha: None,
        },
    )
    .await
    .expect("commit");

    let views = ViewCache::default();
    let stored = views
        .view(&pool, key)
        .await
        .expect("view")
        .expect("a graph");
    assert_eq!(stored.restricted.len(), 1);

    // The listed user reads everything: no filtered copy is made.
    assert!(stored.for_caller(&caller(Some("7"))).is_none());

    for outsider in [caller(Some("8")), caller(None)] {
        let view = stored.for_caller(&outsider).expect("a filtered view");
        assert!(
            view.node("a").is_none(),
            "only the secret document cited it"
        );
        let b = view.node("b").expect("cited by the public document too");
        assert_eq!(b["citations"].as_array().map(Vec::len), Some(1));
        assert_eq!(b["citations"][0]["file_path"], json!("hr/public.md"));
        assert!(view.graph.edge("a", "c").is_none());
        assert!(view.graph.edge("b", "c").is_some());
        let community = &view.graph.metadata["community_data"]["communities"]["community_0"];
        assert_eq!(community["members"], json!(["b", "c"]));
        assert_eq!(
            community["summary"],
            Value::Null,
            "written from the hidden content"
        );
        assert_ne!(community["label"], json!("Compensation"));

        let mut params = Map::new();
        params.insert("query".to_owned(), json!("salary"));
        let answer = dispatch(&Call {
            tool: "search_graph",
            family: "inventory",
            params: &params,
            view: &view,
        })
        .expect("served")
        .expect("answered");
        assert!(
            !answer["result"]
                .as_str()
                .unwrap_or_default()
                .contains("Salary"),
            "{answer}"
        );
    }

    let mut params = Map::new();
    params.insert("query".to_owned(), json!("salary"));
    let answer = dispatch(&Call {
        tool: "search_graph",
        family: "inventory",
        params: &params,
        view: &stored,
    })
    .expect("served")
    .expect("answered");
    assert!(
        answer["result"]
            .as_str()
            .unwrap_or_default()
            .contains("Salary Bands")
    );
}
