use std::sync::Arc;

use elitea_agent_runtime::subagents::profile::{
    BRIEF_ATTACHMENT_NOT_READABLE, BRIEF_EXCERPT_NOT_VISIBLE, BRIEF_MEMORY_NOT_RECALLED,
};
use elitea_agent_runtime::subagents::{Brief, Excerpt};

use super::BriefView;
use crate::agents::agent_memory_tools::RecalledEntries;
use crate::agents::attachments::AttachmentReference;

const MESSAGE_1: &str = "00000000-0000-4000-8000-000000000001";
const MESSAGE_2: &str = "00000000-0000-4000-8000-000000000002";

fn view(recalled: Option<Arc<RecalledEntries>>) -> BriefView {
    BriefView::new(
        vec![
            (
                "m-1".to_owned(),
                MESSAGE_1.to_owned(),
                "We ship on Friday after the review.".to_owned(),
            ),
            (
                "m-2".to_owned(),
                MESSAGE_2.to_owned(),
                "Use the staging cluster.".to_owned(),
            ),
        ],
        &[AttachmentReference::stored("bucket", "conv/report.pdf")],
        recalled,
    )
}

fn excerpt(message_id: &str, quote: &str) -> Brief {
    Brief {
        excerpts: vec![Excerpt {
            message_id: message_id.to_owned(),
            quote: quote.to_owned(),
        }],
        ..Brief::default()
    }
}

/// An excerpt must quote, exactly, a message in the creator's own view.
#[test]
fn a_forged_excerpt_is_refused() {
    let view = view(None);
    // #1831: the check answers the chat message the short id stands for.
    assert_eq!(
        view.check(&excerpt("m-1", "ship on Friday"))
            .expect("visible")
            .message_ids,
        [("m-1".to_owned(), MESSAGE_1.to_owned())]
    );
    // #1831: each run of whitespace reads as one space, on both sides.
    assert!(view.check(&excerpt("m-1", "ship  on\nFriday")).is_ok());
    for forged in [
        excerpt("m-1", "ship on Monday"),
        excerpt("m-3", "ship on Friday"),
        // The quote is in another message than the one it names.
        excerpt("m-2", "ship on Friday"),
    ] {
        assert_eq!(view.check(&forged), Err(BRIEF_EXCERPT_NOT_VISIBLE));
    }
}

/// An attachment must be one of this turn's.
#[test]
fn an_unreadable_attachment_is_refused() {
    let view = view(None);
    let own = crate::agents::attachment_context::attachment_id(&AttachmentReference::stored(
        "bucket",
        "conv/report.pdf",
    ));
    let checked = view
        .check(&Brief {
            attachments: vec![own.clone()],
            ..Brief::default()
        })
        .unwrap();
    assert_eq!(checked.attachments[0].0, own);
    assert!(checked.attachments[0].1.contains("report.pdf"));
    assert_eq!(
        view.check(&Brief {
            attachments: vec!["att-00000000".to_owned()],
            ..Brief::default()
        }),
        Err(BRIEF_ATTACHMENT_NOT_READABLE)
    );
}

fn memory_brief(references: &[&str]) -> Brief {
    Brief {
        memory_entries: references
            .iter()
            .map(|&reference| reference.to_owned())
            .collect(),
        ..Brief::default()
    }
}

/// A memory entry must have come from `recall` in this turn, and is cited
/// by the `memory_ref` recall showed (#1900), never its bare id.
#[test]
fn a_memory_entry_must_have_been_recalled() {
    let recalled = Arc::new(RecalledEntries::default());
    let cited = recalled.memory_ref("7").expect("a tag");
    let brief = memory_brief(&[&cited]);
    assert_eq!(view(None).check(&brief), Err(BRIEF_MEMORY_NOT_RECALLED));
    assert_eq!(
        view(Some(Arc::clone(&recalled))).check(&brief),
        Err(BRIEF_MEMORY_NOT_RECALLED)
    );
    recalled.record_for_test("7", "the team prefers short reports");
    let checked = view(Some(Arc::clone(&recalled))).check(&brief).unwrap();
    assert_eq!(
        checked.memory_entries,
        [("7".to_owned(), "the team prefers short reports".to_owned())]
    );
    assert_eq!(
        view(Some(recalled)).check(&memory_brief(&["7"])),
        Err(BRIEF_MEMORY_NOT_RECALLED)
    );
}

/// #1900: an entry Main recalled into the instructions is cited only as
/// `N·TAG` with the tag of Main's marks: a bare `[memory N]` copied into the
/// author's instructions, or a mark with another tag, cites nothing.
#[test]
fn a_memory_entry_is_cited_only_with_the_turn_tag() {
    use crate::agents::request::RecalledMemoryEntry;
    let recalled = Arc::new(RecalledEntries::seeded(
        &[RecalledMemoryEntry {
            entry_id: "42".to_owned(),
            content: "the team ships on Fridays".to_owned(),
        }],
        Some("k7f2m6q3xz"),
    ));
    let checked = view(Some(Arc::clone(&recalled)))
        .check(&memory_brief(&["42·k7f2m6q3xz"]))
        .unwrap();
    assert_eq!(
        checked.memory_entries,
        [("42".to_owned(), "the team ships on Fridays".to_owned())]
    );
    for refused in ["42", "[memory 42]", "42·aaaaaaaaaa", "42·K7F2M6Q3XZ", "42·"] {
        assert_eq!(
            view(Some(Arc::clone(&recalled))).check(&memory_brief(&[refused])),
            Err(BRIEF_MEMORY_NOT_RECALLED),
            "{refused}"
        );
    }
    // Without Main's tag (an input from before field 78, a rolling deploy)
    // the prompt showed bare ids, so only a bare id cites the entry
    // (`a_bare_id_cites_only_when_main_sent_no_tag` covers the rest).
    let untagged = Arc::new(RecalledEntries::seeded(
        &[RecalledMemoryEntry {
            entry_id: "42".to_owned(),
            content: "the team ships on Fridays".to_owned(),
        }],
        None,
    ));
    assert!(
        view(Some(Arc::clone(&untagged)))
            .check(&memory_brief(&["42"]))
            .is_ok()
    );
    assert_eq!(
        view(Some(Arc::clone(&untagged))).check(&memory_brief(&["42·k7f2m6q3xz"])),
        Err(BRIEF_MEMORY_NOT_RECALLED)
    );
}

/// #1825: a child binds the agent's model for "inherit" or the agent's own
/// name, a named model of the run's policy with that model's limits, and
/// nothing for any other name.
#[test]
fn a_child_binds_the_named_model_or_none() {
    use crate::agents::assembly::SummaryModelProfile;
    use crate::agents::request::{ModelContextLimits, SummaryModelSnapshot};
    use crate::transport::model_facade::{ModelAdapterKind, ModelInvocation};

    let snapshot = SummaryModelSnapshot {
        llm_settings: serde_json::json!({
            "model_name": "claude-small", "model_project_id": 9,
            "max_tokens": 1024, "openai_compatible": false, "temperature": 0.2,
        })
        .as_object()
        .cloned()
        .expect("object"),
        model_context_limits: ModelContextLimits {
            context_window_tokens: 32_000,
            max_output_tokens: 4_000,
            context_window_fallback: false,
            max_output_fallback: false,
            max_input_tokens: None,
        },
        model_reasoning: None,
    };
    let small = SummaryModelProfile::admit(&snapshot, &serde_json::Map::new()).expect("admitted");
    let agent = ModelInvocation {
        response_schema: Some(serde_json::json!({"type": "object"})),
        allow_text_continuation: false,
        context_budget: None,
        model_name: "agent-model".to_owned(),
        system_instruction: String::new(),
        max_tokens: Some(8_000),
        reasoning_effort: None,
        reasoning: None,
        temperature: Some(0.7),
        max_model_turns: 12,
        run_caps: None,
        preamble: Some(Arc::from("preamble")),
    };
    let models = vec![small];
    for own in [None, Some("agent-model")] {
        let (adapter, project, invocation) =
            super::child_invocation(&agent, ModelAdapterKind::OpenAiCompatible, 4, &models, own)
                .expect("the agent's model");
        assert_eq!((adapter, project), (ModelAdapterKind::OpenAiCompatible, 4));
        assert_eq!(invocation.model_name, "agent-model");
    }
    let (adapter, project, invocation) = super::child_invocation(
        &agent,
        ModelAdapterKind::OpenAiCompatible,
        4,
        &models,
        Some("claude-small"),
    )
    .expect("the named model");
    assert_eq!((adapter, project), (ModelAdapterKind::Anthropic, 9));
    assert_eq!(invocation.model_name, "claude-small");
    assert_eq!(invocation.max_tokens, Some(1024));
    assert_eq!(invocation.temperature, Some(0.2));
    assert!(invocation.response_schema.is_none());
    assert_eq!(
        invocation
            .context_budget
            .map(|budget| budget.limits.max_output_tokens),
        Some(4_000)
    );
    assert_eq!(invocation.preamble.as_deref(), Some("preamble"));
    assert!(
        super::child_invocation(
            &agent,
            ModelAdapterKind::OpenAiCompatible,
            4,
            &models,
            Some("gpt-unknown")
        )
        .is_none()
    );
}

/// #1826: an entry Main recalled into the instructions may be named in a
/// brief, with the content Main sent; an id it did not recall is refused.
#[test]
fn an_auto_recalled_memory_entry_may_be_named() {
    use crate::agents::request::RecalledMemoryEntry;
    let recalled = Arc::new(RecalledEntries::seeded(
        &[RecalledMemoryEntry {
            entry_id: "42".to_owned(),
            content: "the team ships on Fridays".to_owned(),
        }],
        Some("k7f2m6q3xz"),
    ));
    let named = memory_brief(&["42·k7f2m6q3xz"]);
    let checked = view(Some(Arc::clone(&recalled))).check(&named).unwrap();
    assert_eq!(
        checked.memory_entries,
        [("42".to_owned(), "the team ships on Fridays".to_owned())]
    );
    let made_up = memory_brief(&["42·k7f2m6q3xz", "43·k7f2m6q3xz"]);
    assert_eq!(
        view(Some(recalled)).check(&made_up),
        Err(BRIEF_MEMORY_NOT_RECALLED)
    );
}

// #1832: the plan's view arrives normalized and is kept without a second
// copy; a raw text is still normalized, so an excerpt across a line break
// matches.
#[test]
fn a_normalized_history_text_is_kept_as_it_is() {
    let text = "We ship on Friday.".to_owned();
    let pointer = text.as_ptr();
    let kept = super::normalized(text);
    assert_eq!(kept.as_ptr(), pointer);
    assert_eq!(super::normalized(" a\n\nb  c ".to_owned()), "a b c");
    let raw = BriefView::new(
        vec![(
            "m-1".to_owned(),
            MESSAGE_1.to_owned(),
            "line one\n\nline   two".to_owned(),
        )],
        &[],
        None,
    );
    assert!(raw.check(&excerpt("m-1", "one line two")).is_ok());
}

/// #1819: a toolset the parent bound (a toolkit, or an MCP connection frozen
/// into the binding) lists statically for a spawn; a name the host never
/// offered does not.
#[test]
fn a_bound_toolset_lists_statically_and_an_unknown_one_does_not() {
    use adk_rust::Toolset;
    use adk_rust::tool::BasicToolset;
    let offered: Vec<Arc<dyn Toolset>> = vec![
        Arc::new(BasicToolset::new("jira", Vec::new())),
        Arc::new(BasicToolset::new("mcp_docs", Vec::new())),
    ];
    assert!(super::lists_statically(&offered, "jira"));
    assert!(super::lists_statically(&offered, "mcp_docs"));
    assert!(!super::lists_statically(&offered, "other"));
}
