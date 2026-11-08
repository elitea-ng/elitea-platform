use std::sync::Arc;

use adk_core::{Tool, ToolContext};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Value, json};

use super::*;

struct Fake {
    name: &'static str,
    read_only: bool,
}

#[async_trait]
impl Tool for Fake {
    fn name(&self) -> &str {
        self.name
    }

    fn description(&self) -> &'static str {
        "A fake tool."
    }

    fn is_read_only(&self) -> bool {
        self.read_only
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, _args: Value) -> adk_core::Result<Value> {
        Ok(json!({"ran": self.name}))
    }
}

fn tool(name: &'static str, read_only: bool) -> Arc<dyn Tool> {
    Arc::new(Fake { name, read_only })
}

/// A "gh" toolkit of type "github": two reads and two writes.
fn github() -> Vec<FrozenToolset> {
    vec![FrozenToolset::new(
        "gh",
        vec![
            tool("get_issue", true),
            tool("list_issues", true),
            tool("create_issue", false),
            tool("create_comment", false),
        ],
    )]
}

fn kinds() -> ToolkitKinds {
    ToolkitKinds::from([("gh".to_owned(), "github".to_owned())])
}

fn reference(name: &'static str) -> ToolRef<'static> {
    ToolRef {
        toolkit_name: "gh",
        toolkit_type: Some("github"),
        tool_name: name,
    }
}

fn envelope(input: &ToolEnvelopeInput) -> ToolEnvelope {
    ToolEnvelope::from_input(input).expect("a valid envelope")
}

fn names(toolsets: &[FrozenToolset]) -> Vec<String> {
    toolsets
        .iter()
        .flat_map(|toolset| toolset.tools().iter().map(|tool| tool.name().to_owned()))
        .collect()
}

#[test]
fn keys_match_the_toolkit_by_name_or_type_and_the_wildcard() {
    let by_type = ToolKey::parse("GitHub/Create_Issue").unwrap();
    assert!(by_type.matches(&reference("create_issue")));
    let by_name = ToolKey::parse(" gh / create_issue ").unwrap();
    assert!(by_name.matches(&reference("create_issue")));
    let every = ToolKey::parse("github/*").unwrap();
    assert!(every.matches(&reference("get_issue")));
    assert!(
        !ToolKey::parse("jira/*")
            .unwrap()
            .matches(&reference("get_issue"))
    );
    assert!(!by_type.matches(&reference("get_issue")));
    for bad in [
        "github",
        "/x",
        "github/",
        "a/b/c",
        "github/get_*",
        "git\nhub/x",
    ] {
        assert!(ToolKey::parse(bad).is_err(), "{bad:?} must be refused");
    }
}

#[test]
fn a_malformed_envelope_is_refused() {
    let refused = |input: ToolEnvelopeInput| ToolEnvelope::from_input(&input).is_err();
    assert!(refused(ToolEnvelopeInput {
        tool_limits: vec![("github/x".to_owned(), 0)],
        ..ToolEnvelopeInput::default()
    }));
    assert!(refused(ToolEnvelopeInput {
        tool_limits: vec![("github/x".to_owned(), MAX_CALLS + 1)],
        ..ToolEnvelopeInput::default()
    }));
    assert!(refused(ToolEnvelopeInput {
        max_tool_limit: MAX_CALLS + 1,
        ..ToolEnvelopeInput::default()
    }));
    assert!(refused(ToolEnvelopeInput {
        unattended_tools: vec!["github/x".to_owned()],
        ..ToolEnvelopeInput::default()
    }));
    assert!(refused(ToolEnvelopeInput {
        subagent_tools: vec!["github/x".to_owned()],
        ..ToolEnvelopeInput::default()
    }));
    assert!(refused(ToolEnvelopeInput {
        subagent_tools: vec!["github".to_owned()],
        subagent_tools_set: true,
        ..ToolEnvelopeInput::default()
    }));
    assert_eq!(
        ToolEnvelope::from_input(&ToolEnvelopeInput {
            unattended_tools_allowlist: vec!["github/x".to_owned(); MAX_ENTRIES + 1],
            ..ToolEnvelopeInput::default()
        })
        .err(),
        Some(ToolEnvelopeError::TooLarge)
    );
}

#[test]
fn tool_limits_take_the_lowest_of_every_match_and_the_ceiling() {
    let none = ToolEnvelope::none();
    assert_eq!(none.limit_for(&reference("create_issue")), None);
    let own = envelope(&ToolEnvelopeInput {
        tool_limits: vec![
            ("github/*".to_owned(), 10),
            ("gh/create_issue".to_owned(), 3),
        ],
        ..ToolEnvelopeInput::default()
    });
    assert_eq!(own.limit_for(&reference("create_issue")), Some(3));
    assert_eq!(own.limit_for(&reference("get_issue")), Some(10));
    let ceiling = envelope(&ToolEnvelopeInput {
        tool_limits: vec![("github/create_issue".to_owned(), 30)],
        max_tool_limit: 5,
        ..ToolEnvelopeInput::default()
    });
    // The author cannot raise the platform's ceiling, and the ceiling
    // covers a tool the author did not name.
    assert_eq!(ceiling.limit_for(&reference("create_issue")), Some(5));
    assert_eq!(ceiling.limit_for(&reference("get_issue")), Some(5));
}

#[tokio::test]
async fn a_limited_tool_refuses_the_call_past_its_limit() {
    let limits = envelope(&ToolEnvelopeInput {
        tool_limits: vec![("github/create_issue".to_owned(), 2)],
        ..ToolEnvelopeInput::default()
    });
    let (toolsets, limited) = limit_toolsets(github(), &limits, &kinds());
    assert_eq!(limited, 1);
    // The envelope keeps every tool: a limit never removes or adds one.
    assert_eq!(names(&toolsets), names(&github()));
    let create = toolsets[0]
        .tools()
        .iter()
        .find(|tool| tool.name() == "create_issue")
        .cloned()
        .unwrap();
    assert!(!create.is_read_only(), "flags pass through");
    let ctx: Arc<dyn ToolContext> = Arc::new(SimpleToolContext::new("limit-test"));
    for _ in 0..2 {
        assert!(create.execute(Arc::clone(&ctx), json!({})).await.is_ok());
    }
    let refused = create
        .execute(Arc::clone(&ctx), json!({}))
        .await
        .unwrap_err();
    assert_eq!(refused.code, TOOL_LIMIT_REACHED);
    let get = toolsets[0]
        .tools()
        .iter()
        .find(|tool| tool.name() == "get_issue")
        .cloned()
        .unwrap();
    for _ in 0..5 {
        assert!(get.execute(Arc::clone(&ctx), json!({})).await.is_ok());
    }
}

#[test]
fn unattended_turns_default_to_the_read_only_tools() {
    let (kept, _, dropped) = retain_unattended(github(), &ToolEnvelope::none(), &kinds());
    assert_eq!(names(&kept), ["get_issue", "list_issues"]);
    assert_eq!(dropped, 2);
}

#[test]
fn unattended_tools_only_narrow_and_writes_need_the_platform_allowlist() {
    // The author names a read, a write and a tool the agent does not have.
    let named = ToolEnvelopeInput {
        unattended_tools: vec![
            "github/get_issue".to_owned(),
            "github/create_comment".to_owned(),
            "jira/create_ticket".to_owned(),
        ],
        unattended_tools_set: true,
        ..ToolEnvelopeInput::default()
    };
    let (kept, ..) = retain_unattended(github(), &envelope(&named), &kinds());
    // The read it names stays, the other read goes, the write needs the
    // allowlist, and the absent tool is not added.
    assert_eq!(names(&kept), ["get_issue"]);
    let allowed = ToolEnvelopeInput {
        unattended_tools_allowlist: vec!["github/create_comment".to_owned()],
        ..named
    };
    let (kept, ..) = retain_unattended(github(), &envelope(&allowed), &kinds());
    assert_eq!(names(&kept), ["get_issue", "create_comment"]);
    // The allowlist alone allows nothing the agent did not name.
    let only_allowlist = ToolEnvelopeInput {
        unattended_tools: vec![],
        unattended_tools_set: true,
        unattended_tools_allowlist: vec!["github/*".to_owned()],
        ..ToolEnvelopeInput::default()
    };
    let (kept, ..) = retain_unattended(github(), &envelope(&only_allowlist), &kinds());
    assert!(kept.is_empty(), "an empty list allows no tool");
}

#[test]
fn subagent_tools_default_to_read_only_and_only_narrow() {
    let none = ToolEnvelope::none();
    assert!(!none.grants_subagent_tools());
    assert!(none.admits_subagent(&reference("get_issue"), true));
    assert!(!none.admits_subagent(&reference("create_issue"), false));
    let granted = envelope(&ToolEnvelopeInput {
        subagent_tools: vec!["github/create_issue".to_owned(), "jira/*".to_owned()],
        subagent_tools_set: true,
        ..ToolEnvelopeInput::default()
    });
    assert!(granted.grants_subagent_tools());
    // A write it names may go to a child; a read it does not name may not.
    assert!(granted.admits_subagent(&reference("create_issue"), false));
    assert!(!granted.admits_subagent(&reference("get_issue"), true));
}
