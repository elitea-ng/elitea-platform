//! The index tools inside the I1 indexing families (ADR-0030 I1): the SDK
//! schema gate, the `selected_tools` rule, the frozen catalog, and the three
//! materialization modes — no catalog (Python indexing), a catalog with no
//! authority (a worker without a vector store), a catalog with an authority.

use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::families::index_tools::{
    I1_INDEX_FAMILIES, IndexCatalog, IndexCatalogEntry, IndexToolAuthority, IndexToolCall,
    IndexToolFailure, IndexToolHost, IndexToolName, IndexToolSupport, IndexToolsWithheld,
    all_index_tools, build_index_toolset, doctype_for, is_index_family,
};
use super::materialize::{
    materialize_configured_toolsets_reporting_refusals,
    materialize_configured_toolsets_with_authorities,
};
use super::policy::ToolAdmissionPolicy;
use super::sdk_conformance;
use super::snapshot::FrozenToolSnapshot;

/// One recorded call: the tool, the toolkit id, the doctype, the catalog's
/// names, the arguments.
type RecordedCall = (IndexToolName, u64, String, Vec<String>, Value);

/// Records every call and answers with a canned value per tool.
struct RecordingHost {
    calls: Mutex<Vec<RecordedCall>>,
    fail: bool,
}

#[async_trait]
impl IndexToolHost for RecordingHost {
    async fn call(&self, call: IndexToolCall<'_>) -> Result<Value, IndexToolFailure> {
        let names = call
            .catalog
            .entries()
            .iter()
            .map(|entry| entry.name.clone())
            .collect();
        self.calls.lock().expect("calls").push((
            call.tool,
            call.toolkit_id,
            call.doctype.to_owned(),
            names,
            call.arguments.clone(),
        ));
        if self.fail {
            return Err(IndexToolFailure::new("the vector store refused the call"));
        }
        Ok(match call.tool {
            IndexToolName::ListIndexes => json!("docs,code"),
            IndexToolName::SearchIndex => {
                json!([{"page_content": "x", "metadata": {}, "score": 0.5}])
            }
            IndexToolName::StepbackSearchIndex | IndexToolName::StepbackSummaryIndex => {
                json!("stepped back")
            }
            IndexToolName::RemoveIndex => {
                json!("Index 'docs' has been removed from the vector store.")
            }
        })
    }
}

fn lend(fail: bool) -> (IndexToolAuthority, Arc<RecordingHost>) {
    let host = Arc::new(RecordingHost {
        calls: Mutex::new(Vec::new()),
        fail,
    });
    (
        IndexToolAuthority::new(Arc::clone(&host) as Arc<dyn IndexToolHost>),
        host,
    )
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new()).expect("policy"))
}

fn catalog_value() -> Value {
    json!([
        {"name": "docs", "index_id": "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b", "embedding_model": "text-embedding-3-small", "embedding_dimension": 1536},
        {"name": "code", "index_id": "1c1f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5c", "embedding_model": "text-embedding-3-small", "embedding_dimension": 1536}
    ])
}

fn github_tool(indexes: Option<Value>, selected: &[&str]) -> Value {
    let mut tool = json!({
        "id": 41,
        "type": "github",
        "toolkit_name": "repo",
        "settings": {
            "github_configuration": {
                "configuration_type": "github",
                "base_url": "https://github.example.test/api/v3/",
                "access_token": "fixture-token"
            },
            "repository": "https://github.example.test/EliteaAI/elitea-platform.git",
            "active_branch": "main",
            "base_branch": "main",
            "selected_tools": selected,
            "pgvector_configuration": null,
            "embedding_model": null
        }
    });
    if let Some(indexes) = indexes {
        tool["indexes"] = indexes;
    }
    tool
}

async fn tool_names(toolsets: &[Arc<dyn Toolset>]) -> Vec<String> {
    let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new("index-tools-test"));
    let mut names = Vec::new();
    for toolset in toolsets {
        for tool in toolset.tools(Arc::clone(&context)).await.expect("tools") {
            names.push(tool.name().to_owned());
        }
    }
    names
}

const INDEX_TOOL_NAMES: [&str; 5] = [
    "list_indexes",
    "search_index",
    "stepback_search_index",
    "stepback_summary_index",
    "remove_index",
];

#[test]
fn the_index_tools_keep_the_sdk_schema_contract_in_every_i1_family() {
    let (authority, _) = lend(false);
    for family in I1_INDEX_FAMILIES {
        let tools = all_index_tools("kit", family, &authority);
        let served: Vec<&str> = tools.iter().map(|tool| tool.name()).collect();
        assert_eq!(served, INDEX_TOOL_NAMES, "{family}");
        let violations = sdk_conformance::violations(family, &tools);
        assert!(
            violations.is_empty(),
            "{family}: the index tools break the SDK contract: {violations:#?}"
        );
    }
}

#[test]
fn names_doctypes_and_read_only_follow_the_sdk() {
    for family in ["github", "gitlab", "bitbucket", "ado_repos"] {
        assert_eq!(doctype_for(family), "code");
        assert!(is_index_family(family));
    }
    assert_eq!(doctype_for("artifact"), "document");
    assert!(is_index_family("artifact"));
    assert!(!is_index_family("jira"), "jira waits for I2");
    for tool in IndexToolName::ALL {
        assert_eq!(IndexToolName::parse(tool.as_str()), Some(tool));
        assert_eq!(tool.is_read_only(), tool != IndexToolName::RemoveIndex);
        assert_eq!(
            tool.calls_model(),
            matches!(
                tool,
                IndexToolName::StepbackSearchIndex | IndexToolName::StepbackSummaryIndex
            )
        );
    }
    assert_eq!(
        IndexToolName::parse("index_data"),
        None,
        "index_data is Main's"
    );
    assert!(
        IndexToolName::SearchIndex
            .sdk_description()
            .starts_with("Searches indexed documents in the vector store.\n\n        Args:")
    );
    assert_eq!(
        IndexToolName::StepbackSearchIndex.sdk_description(),
        " Searches indexed documents in the vector store."
    );
}

#[test]
fn the_search_schema_names_every_sdk_argument_and_requires_only_the_query() {
    let schema = IndexToolName::SearchIndex.parameters_schema();
    let properties = schema["properties"].as_object().expect("properties");
    let mut names: Vec<&str> = properties.keys().map(String::as_str).collect();
    names.sort_unstable();
    assert_eq!(
        names,
        [
            "cut_off",
            "extended_search",
            "filter",
            "full_text_search",
            "index_name",
            "output_fields",
            "query",
            "reranker",
            "reranking_config",
            "search_top",
        ]
    );
    assert_eq!(schema["required"], json!(["query"]));
    assert_eq!(properties["cut_off"]["default"], json!(0.1));
    assert_eq!(properties["search_top"]["default"], json!(10));
    assert_eq!(properties["index_name"]["maxLength"], json!(32));
    let stepback = IndexToolName::StepbackSummaryIndex.parameters_schema();
    let stepback_properties = stepback["properties"].as_object().expect("properties");
    assert!(stepback_properties.contains_key("messages"));
    assert!(!stepback_properties.contains_key("output_fields"));
    assert_eq!(
        IndexToolName::ListIndexes.parameters_schema()["properties"],
        json!({})
    );
    assert_eq!(
        IndexToolName::RemoveIndex.parameters_schema()["properties"]["index_name"]["default"],
        json!("")
    );
}

#[test]
fn the_catalog_is_read_strictly() {
    let catalog = IndexCatalog::parse(&catalog_value()).expect("catalog");
    assert_eq!(catalog.entries().len(), 2);
    assert_eq!(
        catalog.entries()[0],
        IndexCatalogEntry {
            name: "docs".to_owned(),
            index_id: "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b".to_owned(),
            embedding_model: "text-embedding-3-small".to_owned(),
            embedding_dimension: 1536,
        }
    );
    assert!(
        IndexCatalog::parse(&json!([]))
            .expect("empty")
            .entries()
            .is_empty()
    );
    for invalid in [
        json!({}),
        json!([1]),
        json!([{"name": "", "index_id": "a", "embedding_model": "m", "embedding_dimension": 1}]),
        json!([{"name": "n", "index_id": "A-UPPER", "embedding_model": "m", "embedding_dimension": 1}]),
        json!([{"name": "n", "index_id": "ab", "embedding_model": "m", "embedding_dimension": 0}]),
        json!([{"name": "n", "index_id": "ab", "embedding_model": "m", "embedding_dimension": 70000}]),
        json!([{"name": "n", "index_id": "ab", "embedding_dimension": 3}]),
        json!([
            {"name": "n", "index_id": "ab", "embedding_model": "m", "embedding_dimension": 3},
            {"name": "n", "index_id": "cd", "embedding_model": "m", "embedding_dimension": 3}
        ]),
    ] {
        assert!(IndexCatalog::parse(&invalid).is_err(), "{invalid}");
    }
}

#[tokio::test]
async fn selected_tools_keep_only_the_named_index_tools() {
    let (authority, _) = lend(false);
    let settings: Map<String, Value> =
        json!({"selected_tools": ["search_index", "list_indexes", "get_me"]})
            .as_object()
            .expect("settings")
            .clone();
    let toolset = build_index_toolset(
        "repo",
        41,
        "github",
        &settings,
        IndexCatalog::default(),
        &policy(),
        &authority,
    )
    .expect("toolset");
    let names = tool_names(&[Arc::new(toolset) as Arc<dyn Toolset>]).await;
    assert_eq!(names, ["list_indexes", "search_index"]);

    let everything: Map<String, Value> = json!({"selected_tools": []})
        .as_object()
        .expect("settings")
        .clone();
    let toolset = build_index_toolset(
        "repo",
        41,
        "github",
        &everything,
        IndexCatalog::default(),
        &policy(),
        &authority,
    )
    .expect("toolset");
    assert_eq!(
        tool_names(&[Arc::new(toolset) as Arc<dyn Toolset>]).await,
        INDEX_TOOL_NAMES
    );

    let malformed: Map<String, Value> = json!({"selected_tools": "search_index"})
        .as_object()
        .expect("settings")
        .clone();
    assert!(
        build_index_toolset(
            "repo",
            41,
            "github",
            &malformed,
            IndexCatalog::default(),
            &policy(),
            &authority
        )
        .is_err()
    );
}

#[tokio::test]
async fn a_tool_call_reaches_the_host_with_the_catalog_and_a_failure_is_the_result_text() {
    let (authority, host) = lend(false);
    let toolset = build_index_toolset(
        "repo",
        41,
        "github",
        &Map::new(),
        IndexCatalog::parse(&catalog_value()).expect("catalog"),
        &policy(),
        &authority,
    )
    .expect("toolset");
    let context = Arc::new(SimpleToolContext::new("index-tools-test"));
    let tools = toolset
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    let search = tools
        .iter()
        .find(|tool| tool.name() == "search_index")
        .expect("search_index");
    let result = search
        .execute(
            Arc::clone(&context) as Arc<dyn adk_core::ToolContext>,
            json!({"query": "how", "index_name": "docs"}),
        )
        .await
        .expect("result");
    assert_eq!(result[0]["page_content"], "x");
    let recorded = host.calls.lock().expect("calls").clone();
    assert_eq!(recorded.len(), 1);
    assert_eq!(recorded[0].0, IndexToolName::SearchIndex);
    assert_eq!(
        recorded[0].1, 41,
        "the frozen toolkit's id travels with the call"
    );
    assert_eq!(recorded[0].2, "code");
    assert_eq!(recorded[0].3, ["docs", "code"]);
    assert_eq!(recorded[0].4["index_name"], "docs");

    let (failing, _) = lend(true);
    let toolset = build_index_toolset(
        "repo",
        41,
        "github",
        &Map::new(),
        IndexCatalog::default(),
        &policy(),
        &failing,
    )
    .expect("toolset");
    let tools = toolset
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    let remove = tools
        .iter()
        .find(|tool| tool.name() == "remove_index")
        .expect("remove_index");
    assert!(!remove.is_read_only());
    let result = remove
        .execute(
            Arc::clone(&context) as Arc<dyn adk_core::ToolContext>,
            json!({"index_name": "docs"}),
        )
        .await
        .expect("a failure is a result, not an error");
    assert_eq!(result, json!("the vector store refused the call"));
}

#[tokio::test]
async fn without_a_catalog_the_family_has_no_index_tools() {
    // The Python indexing path: Main writes no `indexes`, the SDK worker
    // serves the index tools, and this runtime serves only the family's own.
    let version = json!({"tools": [github_tool(None, &["get_me", "list_branches_in_repo"])]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (authority, host) = lend(false);
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    let names = tool_names(&toolsets).await;
    assert!(names.contains(&"get_me".to_owned()));
    for name in INDEX_TOOL_NAMES {
        assert!(
            !names.contains(&name.to_owned()),
            "{name} must not be served"
        );
    }
    assert!(host.calls.lock().expect("calls").is_empty());
}

#[tokio::test]
async fn with_a_catalog_and_an_authority_the_family_carries_the_index_tools() {
    let mut selected = vec!["get_me", "list_branches_in_repo"];
    selected.extend(INDEX_TOOL_NAMES);
    let version = json!({"tools": [github_tool(Some(catalog_value()), &selected)]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (authority, _) = lend(false);
    let materialized = materialize_configured_toolsets_reporting_refusals(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    // One toolset per configured toolkit, named after it, as before.
    assert_eq!(materialized.toolsets.len(), 1);
    assert_eq!(materialized.toolsets[0].name(), "repo");
    let names = tool_names(&materialized.toolsets).await;
    assert!(names.contains(&"get_me".to_owned()));
    for name in INDEX_TOOL_NAMES {
        assert!(names.contains(&name.to_owned()), "{name} must be served");
    }
    // A stored selection keeps its say over the index tools too.
    let version =
        json!({"tools": [github_tool(Some(catalog_value()), &["get_me", "search_index"])]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    let mut names = tool_names(&toolsets).await;
    names.sort_unstable();
    assert_eq!(names, ["get_me", "search_index"]);
    // A selection of index tools ALONE selects none of the family's own
    // tools and is not an unsupported selection: the toolkit stays.
    let version =
        json!({"tools": [github_tool(Some(catalog_value()), &["search_index", "list_indexes"])]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    let mut names = tool_names(&toolsets).await;
    names.sort_unstable();
    assert_eq!(names, ["list_indexes", "search_index"]);
}

#[tokio::test]
async fn a_catalog_without_an_authority_leaves_the_index_tools_out() {
    let version = json!({"tools": [github_tool(Some(catalog_value()), &["get_me", "list_branches_in_repo"])]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Withheld(IndexToolsWithheld::NoVectorStore),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    let names = tool_names(&toolsets).await;
    assert!(names.contains(&"get_me".to_owned()));
    for name in INDEX_TOOL_NAMES {
        assert!(
            !names.contains(&name.to_owned()),
            "{name} must not be served"
        );
    }
}

#[tokio::test]
async fn a_malformed_catalog_fails_the_toolkit_and_a_non_array_is_refused_at_parse() {
    let version = json!({"tools": [github_tool(Some(json!([{"name": "x"}])), &["get_me"])]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (authority, _) = lend(false);
    assert!(
        materialize_configured_toolsets_with_authorities(
            &snapshot,
            &policy(),
            &Map::new(),
            None,
            IndexToolSupport::Lent(&authority),
        )
        .await
        .is_err()
    );
    let version = json!({"tools": [github_tool(Some(json!({"docs": 1})), &["get_me"])]});
    assert!(
        FrozenToolSnapshot::from_version_details(version.as_object().expect("version")).is_err()
    );
}

/// Review round 1, finding 5: an index-only `selected_tools` is admissible
/// ONLY where the index toolset is actually composed beside the family.
/// Without a catalog (Python indexing) or with the authority withheld, the
/// family keeps refusing it as an unsupported selection and the toolkit is
/// skipped, as before this stage.
#[tokio::test]
async fn an_index_only_selection_is_accepted_only_when_the_index_toolset_is_composed() {
    let (authority, _) = lend(false);
    for (indexes, support, served) in [
        (
            Some(catalog_value()),
            IndexToolSupport::Lent(&authority),
            true,
        ),
        (
            Some(catalog_value()),
            IndexToolSupport::Withheld(IndexToolsWithheld::NoVectorStore),
            false,
        ),
        (None, IndexToolSupport::Lent(&authority), false),
    ] {
        let version = json!({"tools": [github_tool(indexes, &["search_index", "list_indexes"])]});
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
                .expect("snapshot")
                .apply_policy(policy().as_ref());
        let (toolsets, _) = materialize_configured_toolsets_with_authorities(
            &snapshot,
            &policy(),
            &Map::new(),
            None,
            support,
        )
        .await
        .expect("materialized");
        if served {
            assert_eq!(toolsets.len(), 1);
            let mut names = tool_names(&toolsets).await;
            names.sort_unstable();
            assert_eq!(names, ["list_indexes", "search_index"]);
        } else {
            assert!(
                toolsets.is_empty(),
                "an index-only selection without the index toolset is the unsupported selection it always was"
            );
        }
    }
    // The ADO families that are not indexing families keep refusing it
    // whatever the toolkit carries: a catalog on `ado_boards` is not a
    // catalog this stage reads.
    let version = json!({"tools": [{
        "id": 7,
        "type": "ado_boards",
        "toolkit_name": "boards",
        "indexes": catalog_value(),
        "settings": {
            "ado_configuration": {
                "organization_url": "https://dev.azure.com/contoso/",
                "token": "fixture-token"
            },
            "project": "Fabrikam Fiber",
            "limit": 5,
            "selected_tools": ["search_index"]
        }
    }]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    assert!(toolsets.is_empty(), "ado_boards never composes index tools");
}

/// Review round 1, finding 4: on a path that lends no artifact authority
/// (`toolkit.call_tool.v1`), an `artifact` toolkit with a catalog still
/// serves its index tools; the family's own tools are the ones left out.
#[tokio::test]
async fn an_artifact_toolkit_without_an_artifact_authority_still_serves_its_index_tools() {
    let (authority, host) = lend(false);
    let version = json!({"tools": [{
        "id": 9,
        "type": "artifact",
        "toolkit_name": "files",
        "indexes": catalog_value(),
        "settings": {"bucket": "docs", "selected_tools": []}
    }]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    assert_eq!(toolsets[0].name(), "files");
    let mut names = tool_names(&toolsets).await;
    names.sort_unstable();
    let mut expected: Vec<&str> = INDEX_TOOL_NAMES.to_vec();
    expected.sort_unstable();
    assert_eq!(
        names, expected,
        "index tools only, no artifact family tools"
    );
    // The call carries the artifact toolkit's id and doctype.
    let context = Arc::new(SimpleToolContext::new("index-tools-test"));
    let tools = toolsets[0]
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    let list = tools
        .iter()
        .find(|tool| tool.name() == "list_indexes")
        .expect("list_indexes");
    list.execute(
        Arc::clone(&context) as Arc<dyn adk_core::ToolContext>,
        json!({}),
    )
    .await
    .expect("result");
    let recorded = host.calls.lock().expect("calls").clone();
    assert_eq!(recorded[0].1, 9);
    assert_eq!(recorded[0].2, "document");
    // Without a catalog the artifact family on such a path is skipped as
    // before (#906): no authority, no family, nothing to serve.
    let version = json!({"tools": [{
        "id": 9,
        "type": "artifact",
        "toolkit_name": "files",
        "settings": {"bucket": "docs", "selected_tools": []}
    }]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    assert!(toolsets.is_empty());
}

/// The withheld reasons name their cause; only a missing dependency is a
/// warning.
#[test]
fn withheld_reasons_name_the_cause() {
    assert_eq!(
        IndexToolsWithheld::NoVectorStore.reason_code(),
        "no_vector_target"
    );
    assert_eq!(
        IndexToolsWithheld::NoVectorToken.reason_code(),
        "no_vector_token"
    );
    assert_eq!(
        IndexToolsWithheld::NoRuntimeContext.reason_code(),
        "no_runtime_context"
    );
    assert_eq!(
        IndexToolsWithheld::NotServedOnThisPath.reason_code(),
        "not_served_on_this_path"
    );
    assert!(IndexToolsWithheld::NoVectorStore.is_warning());
    assert!(IndexToolsWithheld::NoVectorToken.is_warning());
    assert!(!IndexToolsWithheld::NoRuntimeContext.is_warning());
    assert!(!IndexToolsWithheld::NotServedOnThisPath.is_warning());
    assert_eq!(
        IndexToolsWithheld::NotRequested.reason_code(),
        "not_requested"
    );
    assert!(!IndexToolsWithheld::NotRequested.is_warning());
    let (authority, _) = lend(false);
    assert!(
        IndexToolSupport::lent_or(Some(&authority), IndexToolsWithheld::NoVectorStore)
            .authority()
            .is_some()
    );
    assert!(
        IndexToolSupport::lent_or(None, IndexToolsWithheld::NoVectorStore)
            .authority()
            .is_none()
    );
}
