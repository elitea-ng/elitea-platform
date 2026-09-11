use serde_json::{Map, Value, json};

use super::snapshot::{
    FrozenToolKind, FrozenToolReference, FrozenToolSnapshot, FrozenToolSnapshotErrorCode,
};
use crate::agents::request::{
    AgentExecutionKind, AgentExecutionPayload, AgentExecutionRequest, AgentInputBinding,
    NextInputSuggestionPolicy, UserInput,
};

fn request(kind: AgentExecutionKind, tools: Vec<Value>) -> AgentExecutionRequest {
    let application = match kind {
        AgentExecutionKind::Application => object(&json!({
            "id": 11,
            "version_id": 22,
            "variables": [],
            "version_details": {"tools": tools}
        })),
        AgentExecutionKind::Adhoc => object(&json!({"instructions": "answer carefully"})),
    };
    let top_level_tools = match kind {
        AgentExecutionKind::Application => Vec::new(),
        AgentExecutionKind::Adhoc => tools,
    };
    AgentExecutionRequest {
        kind,
        binding: AgentInputBinding {
            input_bundle_id: "bundle-1".to_owned(),
            input_bundle_digest: [1; 32],
            request_entry_id: "request-1".to_owned(),
            request_immutable_version: "version-1".to_owned(),
            request_content_digest: [2; 32],
        },
        payload: AgentExecutionPayload {
            llm: Map::new(),
            chat_history: Vec::new(),
            user_input: UserInput::Text("question".to_owned()),
            thread_id: Some("thread-1".to_owned()),
            checkpoint_id: None,
            debug: false,
            tools: top_level_tools,
            application,
            internal_tools: Vec::new(),
            steps_limit: None,
            mcp_tokens: Map::new(),
            ignored_mcp_servers: Vec::new(),
            user_declined_mcp_servers: Vec::new(),
            should_continue: false,
            hitl_resume: false,
            hitl_action: None,
            hitl_value: None,
            hitl_decisions: Vec::new(),
            execution_generation: Some("generation-1".to_owned()),
            is_regenerate: false,
            meta: Map::new(),
            conversation_id: Some("conversation-1".to_owned()),
            persona: "generic".to_owned(),
            context_settings: Map::new(),
            supports_vision: false,
            return_chat_history: false,
            invoked_skills: Vec::new(),
            applied_skills: Vec::new(),
            auto_approve_sensitive_actions: false,
            attached_skills: Vec::new(),
            input_attachments: Vec::new(),
            parallel_reconcile: None,
            parallel_terminal_errors: Vec::new(),
            exception_handling_enabled: None,
            debug_mode: None,
            next_input_suggestion: NextInputSuggestionPolicy::default(),
            toolkit_guardrails: None,
            truncated_content: None,
        },
    }
}

fn object(value: &Value) -> Map<String, Value> {
    value.as_object().cloned().expect("fixture object")
}

fn configured_tool(id: u64, tool_type: &str, toolkit_name: &str) -> Value {
    json!({
        "id": id,
        "type": tool_type,
        "name": toolkit_name,
        "description": "Current toolkit",
        "author_id": 11,
        "settings": {
            "selected_tools": ["search", "read"],
            "credential": {
                "__elitea_configuration_ref__": true,
                "configuration_project_id": 7,
                "configuration_type": tool_type,
                "configuration_uuid": "configuration-1",
                "token": "{{secret.RUNTIME_TOKEN}}"
            }
        },
        "meta": {"current": true},
        "is_pinned": false,
        "toolkit_name": toolkit_name
    })
}

#[test]
fn main_application_snapshot_classifies_generic_mcp_and_stored_application_references() {
    let tools = vec![
        configured_tool(19, "sharepoint", "team_docs"),
        json!({
            "id": 52,
            "type": "mcp",
            "name": "documentation-mcp",
            "description": "Saved external MCP server",
            "author_id": 11,
            "settings": {
                "url": "https://mcp.example.invalid/events",
                "selected_tools": ["search_docs"]
            },
            "meta": {"mcp": true},
            "is_pinned": false,
            "toolkit_name": "documentation-mcp"
        }),
        json!({
            "id": 44,
            "type": "application",
            "name": "nested-agent",
            "description": null,
            "author_id": 11,
            "settings": {"application_id": 3, "application_version_id": 4},
            "meta": {},
            "created_at": "2026-08-07T10:00:00Z",
            "toolkit_name": "nested-agent",
            "author": null,
            "agent_type": "agent",
            "online": null,
            "icon_meta": null,
            "variables": [],
            "is_pinned": false,
            "indexes_count": null
        }),
    ];

    let request = request(AgentExecutionKind::Application, tools);
    let snapshot = FrozenToolSnapshot::from_request(&request).expect("frozen tool snapshot");
    let references = snapshot.iter().collect::<Vec<_>>();

    assert_eq!(snapshot.len(), 3);
    assert_eq!(references[0].kind(), FrozenToolKind::Configured);
    assert_eq!(references[0].tool_id(), Some(19));
    assert_eq!(references[0].tool_type(), "sharepoint");
    assert_eq!(references[0].toolkit_name(), "team_docs");
    assert_eq!(references[1].kind(), FrozenToolKind::Mcp);
    assert_eq!(references[1].tool_id(), Some(52));
    assert_eq!(references[1].toolkit_name(), "documentation-mcp");
    assert_eq!(references[2].kind(), FrozenToolKind::Application);
    assert_eq!(references[2].tool_id(), Some(44));
    assert_eq!(references[2].application_identity(), Some((3, 4)));
}

#[test]
fn main_adhoc_snapshot_preserves_identity_only_application_reference() {
    let child = json!({
        "type": "application",
        "name": "release-notes",
        "description": "Read-only child agent",
        "author_id": 11,
        "participant_id": 29,
        "project_id": 7,
        "settings": {
            "variables": [],
            "application_id": 3,
            "selected_tools": [],
            "application_version_id": 4
        },
        "id": null,
        "toolkit_name": "release-notes",
        "agent_type": "openai",
        "created_at": "2026-08-04T10:00:00Z"
    });
    let request = request(AgentExecutionKind::Adhoc, vec![child]);

    let snapshot = FrozenToolSnapshot::from_request(&request).expect("ad-hoc tool snapshot");
    let reference = snapshot.iter().next().expect("application reference");

    assert_eq!(reference.kind(), FrozenToolKind::Application);
    assert_eq!(reference.tool_id(), None);
    assert_eq!(reference.toolkit_name(), "release-notes");
    assert_eq!(reference.application_identity(), Some((3, 4)));
}

#[test]
fn first_non_null_toolkit_id_wins_like_the_current_sdk() {
    let request = request(
        AgentExecutionKind::Adhoc,
        vec![
            configured_tool(19, "sharepoint", "first"),
            configured_tool(19, "github", "duplicate"),
            configured_tool(20, "github", "second"),
        ],
    );

    let snapshot = FrozenToolSnapshot::from_request(&request).expect("deduplicated snapshot");
    let names = snapshot
        .iter()
        .map(FrozenToolReference::toolkit_name)
        .collect::<Vec<_>>();

    assert_eq!(names, ["first", "second"]);
}

#[test]
fn request_kind_selects_one_unambiguous_tool_source() {
    let mut application = request(AgentExecutionKind::Application, Vec::new());
    application
        .payload
        .tools
        .push(configured_tool(19, "sharepoint", "wrong-source"));
    assert_eq!(
        FrozenToolSnapshot::from_request(&application)
            .err()
            .expect("application top-level tools must fail")
            .code(),
        FrozenToolSnapshotErrorCode::InvalidInput
    );

    let mut adhoc = request(AgentExecutionKind::Adhoc, Vec::new());
    adhoc.payload.application.insert(
        "version_details".to_owned(),
        json!({"tools": [configured_tool(19, "sharepoint", "wrong-source")]}),
    );
    assert_eq!(
        FrozenToolSnapshot::from_request(&adhoc)
            .err()
            .expect("ad-hoc nested tools must fail")
            .code(),
        FrozenToolSnapshotErrorCode::InvalidInput
    );
}

#[test]
fn malformed_or_secret_bearing_shapes_fail_without_data_in_diagnostics() {
    let invalid = vec![
        json!("not-an-object"),
        json!({"id": 1, "type": "github", "settings": {}}),
        json!({"id": 1, "type": "github\n", "toolkit_name": "github", "settings": {}}),
        json!({"id": 0, "type": "github", "toolkit_name": "github", "settings": {}}),
        json!({
            "id": 1,
            "type": "github",
            "toolkit_name": "github",
            "settings": {"selected_tools": ["ok", 7]}
        }),
        json!({
            "id": 44,
            "type": "application",
            "name": "nested-agent",
            "description": null,
            "author_id": 11,
            "settings": {
                "application_id": 3,
                "application_version_id": 4,
                "credential": "plaintext-must-not-cross"
            },
            "meta": {},
            "created_at": "2026-08-07T10:00:00Z",
            "toolkit_name": "nested-agent",
            "agent_type": "agent",
            "variables": [],
            "is_pinned": false
        }),
    ];

    for value in invalid {
        let request = request(AgentExecutionKind::Adhoc, vec![value]);
        let error = FrozenToolSnapshot::from_request(&request)
            .err()
            .expect("malformed snapshot");
        assert_eq!(error.code(), FrozenToolSnapshotErrorCode::InvalidInput);
        let diagnostics = format!("{error:?} {error}");
        assert!(!diagnostics.contains("plaintext-must-not-cross"));
    }
}

#[test]
fn tool_count_and_selection_lists_are_explicitly_bounded() {
    let oversized_snapshot_request = request(
        AgentExecutionKind::Adhoc,
        (1..=1_025)
            .map(|id| configured_tool(id, "github", "github"))
            .collect(),
    );
    assert_eq!(
        FrozenToolSnapshot::from_request(&oversized_snapshot_request)
            .err()
            .expect("tool count must be bounded")
            .code(),
        FrozenToolSnapshotErrorCode::ResourceExhausted
    );

    let selected = (0..1_025)
        .map(|index| Value::String(format!("tool-{index}")))
        .collect::<Vec<_>>();
    let oversized_selection_request = request(
        AgentExecutionKind::Adhoc,
        vec![json!({
            "id": 1,
            "type": "github",
            "toolkit_name": "github",
            "settings": {"selected_tools": selected}
        })],
    );
    assert_eq!(
        FrozenToolSnapshot::from_request(&oversized_selection_request)
            .err()
            .expect("selection count must be bounded")
            .code(),
        FrozenToolSnapshotErrorCode::ResourceExhausted
    );
}

fn internal_builder() -> Value {
    json!({
        "id": null,
        "type": "mcp_elitea_internal_skills",
        "name": "Elitea Skills",
        "toolkit_name": "Elitea Skills",
        "settings": {"server_name": "mcp_elitea_internal_skills"},
        "meta": {"mcp": true, "internal_builder": true}
    })
}

#[test]
fn internal_builder_without_saved_toolkit_survives_both_chat_snapshots() {
    for kind in [AgentExecutionKind::Application, AgentExecutionKind::Adhoc] {
        let request = request(kind, vec![internal_builder()]);
        let snapshot = FrozenToolSnapshot::from_request(&request).unwrap();
        let reference = snapshot.iter().next().unwrap();
        assert_eq!(reference.kind(), FrozenToolKind::Mcp);
        assert_eq!(reference.tool_id(), None);
        assert!(reference.is_internal_builder());
    }
}

#[test]
fn missing_saved_id_requires_the_exact_internal_builder_contract() {
    let cases = [
        ("type", json!("mcp_untrusted")),
        ("type", json!("mcp")),
        ("name", json!("Other Skills")),
        ("toolkit_name", json!("Other Skills")),
        ("meta", json!({"mcp": true})),
        (
            "settings",
            json!({"server_name": "mcp_elitea_internal_secrets"}),
        ),
        ("id", json!(0)),
    ];
    for (key, value) in cases {
        let mut descriptor = internal_builder();
        descriptor[key] = value;
        let request = request(AgentExecutionKind::Adhoc, vec![descriptor]);
        assert!(
            FrozenToolSnapshot::from_request(&request).is_err(),
            "accepted {key}"
        );
    }
}
