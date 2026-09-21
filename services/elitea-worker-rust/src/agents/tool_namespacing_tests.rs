//! #983: what happens when two toolsets on one agent publish the same tool.
//!
//! The defect these cover ends the turn ~75 ms after
//! `agent_native_assembly_completed`, with no message naming the collision or
//! either connection, so every assertion here is about the two things a person
//! could not see: that both tools survive under distinguishable names, and
//! that the run says so.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::tool::{BasicToolset, SimpleToolContext};
use adk_rust::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::tool_namespacing::{
    apply_tool_namespacing, plan_from_published_names, plan_tool_namespacing,
    renamed_tools_notice_text,
};

/// One tool that answers with its own identity, so a routed call proves WHICH
/// toolset served it rather than merely that something answered.
struct IdentifyingTool {
    name: String,
    owner: String,
}

#[async_trait]
impl Tool for IdentifyingTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &'static str {
        "a mock tool"
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({"type": "object", "properties": {}}))
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, _args: Value) -> adk_rust::Result<Value> {
        Ok(json!({"served_by": self.owner, "published_as": self.name}))
    }
}

fn toolset(name: &str, tools: &[&str]) -> Arc<dyn Toolset> {
    let tools = tools
        .iter()
        .map(|tool| {
            Arc::new(IdentifyingTool {
                name: (*tool).to_owned(),
                owner: name.to_owned(),
            }) as Arc<dyn Tool>
        })
        .collect::<Vec<_>>();
    Arc::new(BasicToolset::new(name, tools)) as Arc<dyn Toolset>
}

async fn exposed_names(toolsets: &[Arc<dyn Toolset>]) -> Vec<Vec<String>> {
    let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new("test"));
    let mut names = Vec::new();
    for toolset in toolsets {
        let tools = toolset
            .tools(Arc::clone(&context))
            .await
            .expect("the toolset enumerates");
        names.push(tools.iter().map(|tool| tool.name().to_owned()).collect());
    }
    names
}

/// THE DEFECT. Two connections, one tool name: both survive, each under its
/// own connection's name, and nothing else moves.
#[tokio::test]
async fn a_tool_name_two_connections_publish_is_exposed_once_per_connection() {
    let toolsets = vec![
        toolset("Jira MCP", &["echo", "reverse"]),
        toolset("GitHub MCP", &["echo", "search_code"]),
    ];

    let plan = plan_tool_namespacing(&toolsets)
        .await
        .expect("the toolsets enumerate");
    assert!(!plan.is_empty(), "the collision must be planned");

    let exposed = exposed_names(&apply_tool_namespacing(&plan, toolsets)).await;
    assert_eq!(
        exposed,
        vec![
            vec!["jira_mcp__echo".to_owned(), "reverse".to_owned()],
            vec!["github_mcp__echo".to_owned(), "search_code".to_owned()],
        ],
        "only the COLLIDING name is namespaced; the others are untouched"
    );
}

/// The other half of the same rule, and the one that protects every agent that
/// exists today: no collision, no rename, not even a wrapper.
#[tokio::test]
async fn toolsets_that_publish_distinct_names_are_left_exactly_as_they_are() {
    let toolsets = vec![
        toolset("Jira MCP", &["echo"]),
        toolset("GitHub MCP", &["reverse"]),
    ];

    let plan = plan_tool_namespacing(&toolsets)
        .await
        .expect("the toolsets enumerate");
    assert!(plan.is_empty(), "nothing collided, so nothing is renamed");
    assert_eq!(renamed_tools_notice_text(plan.renamed()), None);

    let applied = apply_tool_namespacing(&plan, toolsets.clone());
    assert_eq!(
        exposed_names(&applied).await,
        vec![vec!["echo".to_owned()], vec!["reverse".to_owned()]],
    );
    for (before, after) in toolsets.iter().zip(&applied) {
        assert!(
            Arc::ptr_eq(before, after),
            "an untouched toolset must not even be wrapped"
        );
    }
}

/// A renamed call reaches the toolset that published it — by map, never by
/// parsing the exposed name.
#[tokio::test]
async fn a_renamed_call_is_routed_to_the_toolset_that_published_it() {
    let toolsets = vec![
        toolset("Jira MCP", &["echo"]),
        toolset("GitHub MCP", &["echo"]),
    ];
    let plan = plan_tool_namespacing(&toolsets)
        .await
        .expect("the toolsets enumerate");
    let applied = apply_tool_namespacing(&plan, toolsets);

    let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new("test"));
    let mut served = BTreeMap::new();
    for toolset in &applied {
        for tool in toolset
            .tools(Arc::clone(&context))
            .await
            .expect("the toolset enumerates")
        {
            let ctx: Arc<dyn ToolContext> = Arc::new(SimpleToolContext::new("test"));
            let result = tool
                .execute(ctx, json!({}))
                .await
                .expect("the delegated tool answers");
            served.insert(tool.name().to_owned(), result);
        }
    }

    assert_eq!(
        served
            .get("jira_mcp__echo")
            .and_then(|value| value.get("served_by"))
            .and_then(Value::as_str),
        Some("Jira MCP"),
    );
    assert_eq!(
        served
            .get("github_mcp__echo")
            .and_then(|value| value.get("served_by"))
            .and_then(Value::as_str),
        Some("GitHub MCP"),
    );
    assert_eq!(
        served
            .get("jira_mcp__echo")
            .and_then(|value| value.get("published_as"))
            .and_then(Value::as_str),
        Some("echo"),
        "the tool still calls the server's own operation, not the exposed name"
    );
}

/// The notice is the whole point of the fix from where a person stands: it has
/// to name BOTH connections and BOTH renamed tools, in one line per collision.
#[test]
fn the_notice_names_every_connection_and_every_renamed_tool() {
    let plan = plan_from_published_names(
        &[
            vec!["echo".to_owned()],
            vec!["echo".to_owned(), "reverse".to_owned()],
        ],
        &["Jira MCP".to_owned(), "GitHub MCP".to_owned()],
    );
    let notice = renamed_tools_notice_text(plan.renamed()).expect("a collision produces a notice");
    assert_eq!(
        notice,
        "tool 'echo' is published by more than one connection; \
         call 'jira_mcp__echo' for 'Jira MCP', 'github_mcp__echo' for 'GitHub MCP'",
    );
    assert!(
        !notice.contains("reverse"),
        "a name only one connection publishes is not in the notice"
    );
}

/// Two connections whose LABELS reduce to the same slug must still get two
/// distinguishable names — otherwise the fix reproduces the defect it removes.
#[test]
fn connections_whose_names_reduce_to_the_same_slug_are_still_distinguished() {
    let plan = plan_from_published_names(
        &[vec!["echo".to_owned()], vec!["echo".to_owned()]],
        &["MCP server".to_owned(), "mcp/server".to_owned()],
    );
    let exposed = plan
        .renamed()
        .iter()
        .map(|entry| entry.exposed.clone())
        .collect::<Vec<_>>();
    assert_eq!(exposed, vec!["mcp_server__echo", "mcp_server_2__echo"]);
}

/// The exposed name has to satisfy the strictest provider rule a turn can meet
/// (`^[a-zA-Z0-9_-]{1,64}$`), whatever the stored toolkit label contains.
#[test]
fn an_exposed_name_is_a_legal_function_name_however_long_the_label_is() {
    let label = "Прод ".to_owned() + &"X".repeat(400);
    let plan = plan_from_published_names(
        &[vec!["echo".to_owned()], vec!["echo".to_owned()]],
        &[label, "b".to_owned()],
    );
    for entry in plan.renamed() {
        assert!(
            entry.exposed.len() <= 64 && !entry.exposed.is_empty(),
            "exposed name out of bounds: {}",
            entry.exposed
        );
        assert!(
            entry
                .exposed
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'-'),
            "exposed name is not a legal function name: {}",
            entry.exposed
        );
    }
}

/// A name that stays as published must never be stolen by a namespaced one.
#[test]
fn a_namespaced_name_never_collides_with_a_name_that_stayed() {
    let plan = plan_from_published_names(
        &[
            vec!["echo".to_owned()],
            vec!["echo".to_owned()],
            vec!["a__echo".to_owned()],
        ],
        &["a".to_owned(), "b".to_owned(), "c".to_owned()],
    );
    let exposed = plan
        .renamed()
        .iter()
        .map(|entry| entry.exposed.clone())
        .collect::<Vec<_>>();
    assert!(
        !exposed.contains(&"a__echo".to_owned()),
        "the third toolset already publishes `a__echo`: {exposed:?}"
    );
}
