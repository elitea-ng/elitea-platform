//! The `memory` toolkit family (worker unit M9): the SDK schema gate, the
//! `selected_tools` rule, the namespace rule, and the three materialization
//! modes — an authority that serves memory, one that does not (no memory
//! embedding model), and none at all.

use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::families::index_tools::{
    IndexToolAuthority, IndexToolCall, IndexToolFailure, IndexToolHost, IndexToolSupport,
    IndexToolsWithheld,
};
use super::families::memory::{
    MemoryToolCall, MemoryToolName, MemoryToolsetError, all_memory_tools, build_memory_toolset,
    memory_namespace,
};
use super::materialize::materialize_configured_toolsets_with_authorities;
use super::policy::ToolAdmissionPolicy;
use super::sdk_conformance;
use super::snapshot::{AdmittedToolSnapshot, FrozenToolSnapshot};

type Recorded = (MemoryToolName, String, Value);

struct MemoryHost {
    serves: bool,
    fail: bool,
    calls: Mutex<Vec<Recorded>>,
}

#[async_trait]
impl IndexToolHost for MemoryHost {
    async fn call(&self, _call: IndexToolCall<'_>) -> Result<Value, IndexToolFailure> {
        Err(IndexToolFailure::new("not an index host"))
    }

    fn serves_memory(&self) -> bool {
        self.serves
    }

    async fn memory(&self, call: MemoryToolCall<'_>) -> Result<Value, IndexToolFailure> {
        self.calls.lock().expect("calls").push((
            call.tool,
            call.namespace.to_owned(),
            call.arguments.clone(),
        ));
        if self.fail {
            return Err(IndexToolFailure::new(
                "Failed to store memory: the vector store is unavailable right now; try again in a moment",
            ));
        }
        Ok(json!("done"))
    }
}

fn lend(serves: bool, fail: bool) -> (IndexToolAuthority, Arc<MemoryHost>) {
    let host = Arc::new(MemoryHost {
        serves,
        fail,
        calls: Mutex::new(Vec::new()),
    });
    (
        IndexToolAuthority::new(Arc::clone(&host) as Arc<dyn IndexToolHost>),
        host,
    )
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &std::collections::BTreeMap::new()).expect("policy"))
}

fn settings(value: Value) -> Map<String, Value> {
    let Value::Object(map) = value else {
        panic!("settings are an object");
    };
    map
}

fn memory_tool(settings: &Value) -> Value {
    json!({
        "id": 7,
        "type": "memory",
        "toolkit_name": "notes",
        "settings": settings
    })
}

fn version(tool: &Value) -> Value {
    json!({"tools": [tool]})
}

fn admitted(version: &Value) -> AdmittedToolSnapshot<'_> {
    FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref())
}

async fn tool_names(toolsets: &[Arc<dyn Toolset>]) -> Vec<String> {
    let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new("memory-test"));
    let mut names = Vec::new();
    for toolset in toolsets {
        for tool in toolset.tools(Arc::clone(&context)).await.expect("tools") {
            names.push(tool.name().to_owned());
        }
    }
    names
}

#[test]
fn the_memory_tools_keep_the_sdk_schema_contract() {
    let (authority, _) = lend(true, false);
    let tools = all_memory_tools(&authority);
    let served: Vec<&str> = tools.iter().map(|tool| tool.name()).collect();
    assert_eq!(
        served,
        [
            "manage_memory",
            "search_memory",
            "get_memory",
            "delete_memory"
        ]
    );
    sdk_conformance::assert_sdk_conformance("memory", &tools);
    for tool in &tools {
        let read_only = matches!(tool.name(), "search_memory" | "get_memory");
        assert_eq!(tool.is_read_only(), read_only, "{}", tool.name());
    }
    assert_eq!(
        MemoryToolName::ManageMemory.sdk_description(),
        "Store information in long-term memory. Use this to remember important facts,\nuser preferences, or any information that should persist across conversations."
    );
    let search = MemoryToolName::SearchMemory.parameters_schema();
    assert_eq!(search["properties"]["limit"]["default"], json!(5));
    assert_eq!(search["required"], json!(["query"]));
}

#[test]
fn the_namespace_is_the_setting_or_the_toolkit_id() {
    assert_eq!(
        memory_namespace(&settings(json!({"namespace": "team"})), Some(7)),
        Ok("team".to_owned())
    );
    assert_eq!(memory_namespace(&Map::new(), Some(7)), Ok("7".to_owned()));
    assert_eq!(
        memory_namespace(&settings(json!({"namespace": null})), Some(7)),
        Ok("7".to_owned())
    );
    for bad in [
        json!({"namespace": ""}),
        json!({"namespace": "   "}),
        json!({"namespace": 5}),
        json!({"namespace": "a\nb"}),
        json!({"namespace": "x".repeat(257)}),
    ] {
        assert_eq!(
            memory_namespace(&settings(bad.clone()), Some(7)),
            Err(MemoryToolsetError::Invalid),
            "{bad}"
        );
    }
    assert_eq!(
        memory_namespace(&Map::new(), None),
        Err(MemoryToolsetError::Invalid)
    );
}

#[tokio::test]
async fn selected_tools_follow_the_sdk_rule() {
    let (authority, _) = lend(true, false);
    for (selected, expected) in [
        (
            json!(["manage_memory", "search_memory"]),
            vec!["manage_memory", "search_memory"],
        ),
        (
            json!([]),
            vec![
                "manage_memory",
                "search_memory",
                "get_memory",
                "delete_memory",
            ],
        ),
        (json!(["delete_memory", "unknown"]), vec!["delete_memory"]),
    ] {
        let toolset = build_memory_toolset(
            "notes",
            Some(7),
            &settings(json!({"namespace": "n", "selected_tools": selected})),
            &policy(),
            &authority,
        )
        .expect("toolset");
        assert_eq!(
            tool_names(&[Arc::new(toolset) as Arc<dyn Toolset>]).await,
            expected
        );
    }
    assert!(
        build_memory_toolset(
            "notes",
            Some(7),
            &settings(json!({"selected_tools": "manage_memory"})),
            &policy(),
            &authority,
        )
        .is_err()
    );
}

#[tokio::test]
async fn a_call_reaches_the_host_with_its_namespace_and_a_failure_is_the_result_text() {
    let (authority, host) = lend(true, false);
    let toolset = build_memory_toolset(
        "notes",
        Some(7),
        &settings(json!({"namespace": "team-a"})),
        &policy(),
        &authority,
    )
    .expect("toolset");
    let context = Arc::new(SimpleToolContext::new("memory-test"));
    let tools = toolset
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    let manage = tools
        .iter()
        .find(|tool| tool.name() == "manage_memory")
        .expect("manage_memory");
    let result = manage
        .execute(
            Arc::clone(&context) as Arc<dyn adk_core::ToolContext>,
            json!({"content": "likes tea"}),
        )
        .await
        .expect("result");
    assert_eq!(result, json!("done"));
    let recorded = host.calls.lock().expect("calls").clone();
    assert_eq!(recorded.len(), 1);
    assert_eq!(recorded[0].0, MemoryToolName::ManageMemory);
    assert_eq!(recorded[0].1, "team-a");
    assert_eq!(recorded[0].2["content"], "likes tea");

    let (failing, _) = lend(true, true);
    let toolset =
        build_memory_toolset("notes", Some(7), &Map::new(), &policy(), &failing).expect("toolset");
    let tools = toolset
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    let result = tools[0]
        .execute(
            Arc::clone(&context) as Arc<dyn adk_core::ToolContext>,
            json!({"content": "x"}),
        )
        .await
        .expect("a failure is a result, not an error");
    assert_eq!(
        result,
        json!(
            "Failed to store memory: the vector store is unavailable right now; try again in a moment"
        )
    );
}

#[tokio::test]
async fn materialization_serves_memory_only_under_an_authority_that_serves_it() {
    let tool = memory_tool(&json!({
        "namespace": "n",
        "pgvector_configuration": {"connection_string": "postgresql+psycopg://x"},
        "selected_tools": ["manage_memory", "search_memory"]
    }));

    // Lent, and the host serves memory: the toolkit's selected tools.
    let (authority, _) = lend(true, false);
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &admitted(&version(&tool)),
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    assert_eq!(toolsets[0].name(), "notes");
    assert_eq!(
        tool_names(&toolsets).await,
        ["manage_memory", "search_memory"]
    );

    // Lent, but the host has no memory embedding model: skipped, not failed.
    let (silent, _) = lend(false, false);
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &admitted(&version(&tool)),
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&silent),
    )
    .await
    .expect("materialized");
    assert!(toolsets.is_empty());

    // Withheld for every reason: skipped.
    for reason in [
        IndexToolsWithheld::NoVectorStore,
        IndexToolsWithheld::NoVectorToken,
        IndexToolsWithheld::NoRuntimeContext,
        IndexToolsWithheld::NotServedOnThisPath,
        IndexToolsWithheld::NotRequested,
    ] {
        let (toolsets, _) = materialize_configured_toolsets_with_authorities(
            &admitted(&version(&tool)),
            &policy(),
            &Map::new(),
            None,
            IndexToolSupport::Withheld(reason),
        )
        .await
        .expect("materialized");
        assert!(toolsets.is_empty(), "{reason:?}");
    }

    // The namespace falls back to the toolkit id.
    let (authority, host) = lend(true, false);
    let (toolsets, _) = materialize_configured_toolsets_with_authorities(
        &admitted(&version(&memory_tool(&json!({"selected_tools": []})))),
        &policy(),
        &Map::new(),
        None,
        IndexToolSupport::Lent(&authority),
    )
    .await
    .expect("materialized");
    let context = Arc::new(SimpleToolContext::new("memory-test"));
    let tools = toolsets[0]
        .tools(Arc::clone(&context) as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    tools[2]
        .execute(
            context as Arc<dyn adk_core::ToolContext>,
            json!({"key": "k"}),
        )
        .await
        .expect("get");
    assert_eq!(host.calls.lock().expect("calls")[0].1, "7");
}
