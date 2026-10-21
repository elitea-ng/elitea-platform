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
    INDEX_FAMILIES, IndexCatalog, IndexCatalogEntry, IndexToolAuthority, IndexToolCall,
    IndexToolFailure, IndexToolHost, IndexToolName, IndexToolSupport, IndexToolsWithheld,
    MAX_CATALOG_ENTRIES, SharedIndexCatalog, all_index_tools, build_index_toolset, doctype_for,
    is_index_family,
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
            .snapshot()
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
        if call.tool == IndexToolName::RemoveIndex {
            // What the worker host does once Main reports the index removed.
            let removed = call.arguments["index_name"].as_str().unwrap_or("docs");
            call.catalog.remove(removed);
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
    for family in INDEX_FAMILIES {
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
    for family in [
        "confluence",
        "jira",
        "sharepoint",
        "ado_wiki",
        "ado_boards",
        "ado_plans",
        "figma",
        "testrail",
        "qtest",
        "xray_cloud",
        "zephyr_scale",
        "zephyr_essential",
        "zephyr_enterprise",
    ] {
        assert_eq!(doctype_for(family), "document");
        assert!(is_index_family(family), "{family} is indexed since I2/I3");
    }
    assert!(!is_index_family("aha"), "aha has no indexing toolkit");
    // The SDK's other Zephyr toolkits have no indexer.
    assert!(!is_index_family("zephyr"));
    assert!(!is_index_family("zephyr_squad"));
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
    assert_eq!(
        IndexCatalog::parse(&json!([])).expect("empty").entries(),
        []
    );
    for invalid in [
        json!({}),
        json!([1]),
        json!([{"name": "n", "index_id": "ab", "embedding_dimension": 3}]),
        json!([
            {"name": "n", "index_id": "ab", "embedding_model": "m", "embedding_dimension": 3},
            {"name": "n", "index_id": "cd", "embedding_model": "m", "embedding_dimension": 3}
        ]),
    ] {
        assert!(IndexCatalog::parse(&invalid).is_err(), "{invalid}");
    }
}

/// Review round 2, finding 2: the entry rule is ONE rule, shared with
/// elitea-main's `indexcatalog.ValidEntry` through the fixture both test
/// suites read, and the entry cap is Main's cap.
#[test]
fn the_catalog_entry_rule_is_the_one_main_freezes_by() {
    let fixture: Value =
        serde_json::from_str(include_str!("families/index_catalog_rule_cases.json"))
            .expect("fixture");
    assert_eq!(fixture["max_entries"], json!(MAX_CATALOG_ENTRIES));
    let cases = fixture["cases"].as_array().expect("cases");
    assert!(cases.len() >= 20, "the fixture lost its cases");
    for case in cases {
        let entry = json!([{
            "name": case["name"],
            "index_id": case["index_id"],
            "embedding_model": case["embedding_model"],
            "embedding_dimension": case["embedding_dimension"],
        }]);
        let valid = case["valid"].as_bool().expect("valid");
        assert_eq!(
            IndexCatalog::parse(&entry).is_ok(),
            valid,
            "{}: {entry}",
            case["note"]
        );
    }
    // Exactly the cap parses; one more does not.
    let entry = |n: usize| json!({"name": format!("i{n}"), "index_id": "ab", "embedding_model": "m", "embedding_dimension": 3});
    let full: Vec<Value> = (0..MAX_CATALOG_ENTRIES).map(entry).collect();
    assert!(IndexCatalog::parse(&Value::Array(full.clone())).is_ok());
    let mut over = full;
    over.push(entry(MAX_CATALOG_ENTRIES));
    assert!(IndexCatalog::parse(&Value::Array(over)).is_err());
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

/// Review round 2, finding 2: a catalog Main froze that this runtime cannot
/// read leaves that toolkit's index tools out with a warning; the family's
/// own tools are served. A non-array `indexes` is still refused at the
/// snapshot, before any toolkit is built.
#[tokio::test]
async fn a_malformed_catalog_drops_the_index_tools_and_keeps_the_family() {
    let version = json!({"tools": [github_tool(Some(json!([{"name": "x"}])), &["get_me"])]});
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
    assert_eq!(tool_names(&toolsets).await, ["get_me"]);
    assert!(host.calls.lock().expect("calls").is_empty());
    let version = json!({"tools": [github_tool(Some(json!({"docs": 1})), &["get_me"])]});
    assert!(
        FrozenToolSnapshot::from_version_details(version.as_object().expect("version")).is_err()
    );
}

fn gitlab_tool(indexes: Option<Value>, selected: &[&str]) -> Value {
    let mut tool = json!({
        "id": 42,
        "type": "gitlab",
        "toolkit_name": "release_repository",
        "settings": {
            "gitlab_configuration": {
                "url": "https://gitlab.example.test/",
                "private_token": "fixture-token"
            },
            "repository": "team/project",
            "branch": "main",
            "selected_tools": selected,
            "pgvector_configuration": null
        }
    });
    if let Some(indexes) = indexes {
        tool["indexes"] = indexes;
    }
    tool
}

fn bitbucket_tool(indexes: Option<Value>, selected: &[&str]) -> Value {
    let mut tool = json!({
        "id": 43,
        "type": "bitbucket",
        "toolkit_name": "app_repository",
        "settings": {
            "bitbucket_configuration": {
                "url": "https://bitbucket.example.test/ctx/",
                "username": "bot",
                "password": "fixture-password"
            },
            "project": "PROJ",
            "repository": "app",
            "branch": "main",
            "cloud": null,
            "selected_tools": selected,
            "pgvector_configuration": null
        }
    });
    if let Some(indexes) = indexes {
        tool["indexes"] = indexes;
    }
    tool
}

/// Review round 2, finding 1: a family half that is skipped (a selection
/// this runtime does not serve) no longer takes the toolkit's index tools
/// down with it. GitHub's port refuses a selection of only
/// `generic_github_api_call` (a `PyGithub` dispatcher it does not serve) and
/// an index tool: `search_index` is still served under the toolkit's name. A GitLab or Bitbucket selection naming
/// a tool the port does not serve (Bitbucket has no `delete_file`; the
/// GitLab name is outside its port) is skipped as a family, and
/// `search_index` beside it is still served. Without an index toolset the
/// same toolkits are omitted, as before.
#[tokio::test]
async fn a_skipped_family_half_keeps_the_toolkits_index_tools() {
    let (authority, _) = lend(false);
    // github, a selection it serves none of: the family is skipped, the index
    // tools stay.
    let version = json!({"tools": [github_tool(Some(catalog_value()), &["generic_github_api_call", "search_index"])]});
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
    assert_eq!(toolsets[0].name(), "repo");
    assert_eq!(tool_names(&toolsets).await, ["search_index"]);
    // The same toolkit without a catalog is omitted, as before this stage.
    let version =
        json!({"tools": [github_tool(None, &["generic_github_api_call", "search_index"])]});
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
    // gitlab and bitbucket with a tool this runtime does not serve.
    for (tool, toolkit_name) in [
        (
            gitlab_tool(
                Some(catalog_value()),
                &["approve_merge_request", "search_index"],
            ),
            "release_repository",
        ),
        (
            bitbucket_tool(Some(catalog_value()), &["delete_file", "search_index"]),
            "app_repository",
        ),
    ] {
        let version = json!({"tools": [tool]});
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
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
        assert_eq!(toolsets.len(), 1, "{toolkit_name}");
        assert_eq!(toolsets[0].name(), toolkit_name);
        assert_eq!(
            tool_names(&toolsets).await,
            ["search_index"],
            "{toolkit_name}"
        );
    }
    // Both are omitted entirely without an index toolset.
    for tool in [
        gitlab_tool(None, &["approve_merge_request", "search_index"]),
        bitbucket_tool(None, &["delete_file", "search_index"]),
    ] {
        let version = json!({"tools": [tool]});
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
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
}

/// Review round 2, finding 5: the toolkit's tools share one catalog for the
/// run, and `remove_index` takes the removed index out of it, so a later
/// `list_indexes` in the same turn no longer sees it.
#[tokio::test]
async fn remove_index_takes_the_index_out_of_the_turns_shared_catalog() {
    let shared = SharedIndexCatalog::new(IndexCatalog::parse(&catalog_value()).expect("catalog"));
    assert!(shared.remove("docs"));
    assert!(!shared.remove("docs"), "already gone");
    assert_eq!(shared.snapshot().entries().len(), 1);
    assert_eq!(shared.snapshot().entries()[0].name, "code");

    let (authority, host) = lend(false);
    let version =
        json!({"tools": [github_tool(Some(catalog_value()), &["list_indexes", "remove_index"])]});
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
    let context = Arc::new(SimpleToolContext::new("index-tools-test"));
    let tools = toolsets[0]
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    let tool =
        |name: &str| Arc::clone(tools.iter().find(|tool| tool.name() == name).expect("tool"));
    for (name, arguments) in [
        ("list_indexes", json!({})),
        ("remove_index", json!({"index_name": "docs"})),
        ("list_indexes", json!({})),
    ] {
        tool(name)
            .execute(
                Arc::clone(&context) as Arc<dyn adk_core::ToolContext>,
                arguments,
            )
            .await
            .expect("result");
    }
    let recorded = host.calls.lock().expect("calls").clone();
    assert_eq!(recorded.len(), 3);
    assert_eq!(recorded[0].3, ["docs", "code"]);
    assert_eq!(
        recorded[1].3,
        ["docs", "code"],
        "remove_index sees the index it removes"
    );
    assert_eq!(
        recorded[2].3,
        ["code"],
        "the removed index is gone for the rest of the turn"
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
    // A family that is not an indexing family (aha) keeps refusing it
    // whatever the toolkit carries: a catalog on `aha` is not a catalog this
    // stage reads.
    let version = json!({"tools": [{
        "id": 7,
        "type": "aha",
        "toolkit_name": "ideas",
        "indexes": catalog_value(),
        "settings": {
            "aha_configuration": {"base_url": "https://acme.aha.io", "api_key": "fixture-key"},
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
    assert!(toolsets.is_empty(), "aha never composes index tools");
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

/// The served families have one source of truth, shared with the Go
/// `indexcatalog` tests and the worker's: a list that drifts fails.
#[test]
fn the_served_families_match_the_shared_fixture() {
    let fixture: serde_json::Value =
        serde_json::from_str(include_str!("families/index_served_families.json"))
            .expect("the fixture is JSON");
    let expected: Vec<&str> = fixture["families"]
        .as_array()
        .expect("families")
        .iter()
        .map(|family| family.as_str().expect("a name"))
        .collect();
    let mut served: Vec<&str> = INDEX_FAMILIES.into_iter().collect();
    served.sort_unstable();
    assert_eq!(served, expected);
    for family in &expected {
        assert!(is_index_family(family), "{family}");
    }
}

/// Review C4: index tools plus a name the family does not serve (jira
/// `["search_index", "made_up"]`) is index-only once `made_up` is pruned. The
/// index tools are served, `made_up` is reported as a tool left out, and no
/// "family not ported" line appears: the family IS ported.
#[tokio::test]
async fn index_tools_with_an_unknown_name_stand_alone_and_report_the_name() {
    use super::materialize::LeftOutReason;
    let (authority, _) = lend(false);
    let version = json!({"tools": [{
        "id": 51,
        "type": "jira",
        "toolkit_name": "tracker",
        "indexes": catalog_value(),
        "settings": {
            "jira_configuration": {
                "base_url": "https://tenant.atlassian.net/",
                "hosting": "Auto",
                "username": "",
                "api_key": null,
                "token": "jira-private-token"
            },
            "api_version": "Auto",
            "limit": 5,
            "verify_ssl": true,
            "selected_tools": ["search_index", "made_up"]
        }
    }]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());
    let materialized = materialize_configured_toolsets_reporting_refusals(
        &snapshot,
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    assert_eq!(materialized.toolsets.len(), 1);
    assert_eq!(tool_names(&materialized.toolsets).await, ["search_index"]);
    assert_eq!(
        materialized.left_out.len(),
        1,
        "{:?}",
        materialized.left_out
    );
    assert_eq!(materialized.left_out[0].tool_name(), Some("made_up"));
    assert_eq!(
        materialized.left_out[0].cause(),
        LeftOutReason::ToolNotServed
    );
}
