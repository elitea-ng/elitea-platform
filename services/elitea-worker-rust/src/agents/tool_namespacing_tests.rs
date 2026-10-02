//! #983: what happens when two toolsets on one agent publish the same tool.
//!
//! The defect these cover ends the turn ~75 ms after
//! `agent_native_assembly_completed`, with no message naming the collision or
//! either connection, so every assertion here is about the two things a person
//! could not see: that both tools survive under distinguishable names, and
//! that the run says so.

use std::collections::BTreeSet;
use std::sync::Arc;

use adk_rust::tool::{BasicToolset, SimpleToolContext};
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::tool_namespacing::{RenamedTool, renamed_tools_notice_text};
use crate::toolkits::bind_toolsets;

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

#[tokio::test]
async fn collisions_keep_exact_dispatch_and_report_the_canonical_aliases() {
    let plan = bind_toolsets(
        vec![
            toolset("first", &["search"]),
            toolset("second", &["search"]),
        ],
        &BTreeSet::new(),
        "merge_alias_test",
    )
    .await
    .expect("canonical bindings");
    let renamed: Vec<_> = plan
        .bindings()
        .map(|(toolkit, original, exposed)| RenamedTool {
            toolkit: toolkit.to_owned(),
            original: original.to_owned(),
            exposed: exposed.to_owned(),
        })
        .collect();
    assert_eq!(renamed.len(), 2);
    assert_ne!(renamed[0].exposed, renamed[1].exposed);
    let notice = renamed_tools_notice_text(&renamed).expect("rename notice");
    for entry in &renamed {
        assert!(notice.contains(&entry.exposed));
        assert!(notice.contains(&entry.toolkit));
    }
    let context = Arc::new(SimpleToolContext::new("merge_alias_test"));
    for set in plan.into_toolsets() {
        for tool in set.tools(context.clone()).await.expect("tools") {
            let result = tool
                .execute(context.clone(), json!({}))
                .await
                .expect("dispatch");
            let owner = renamed
                .iter()
                .find(|entry| entry.exposed == tool.name())
                .expect("owner");
            assert_eq!(result["served_by"], owner.toolkit);
        }
    }
    assert_eq!(renamed_tools_notice_text(&[]), None);
}
