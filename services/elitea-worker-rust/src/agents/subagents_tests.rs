use std::sync::Arc;

use elitea_agent_runtime::subagents::profile::{
    BRIEF_ATTACHMENT_NOT_READABLE, BRIEF_EXCERPT_NOT_VISIBLE, BRIEF_MEMORY_NOT_RECALLED,
};
use elitea_agent_runtime::subagents::{Brief, Excerpt};

use super::BriefView;
use crate::agents::agent_memory_tools::RecalledEntries;
use crate::agents::attachments::AttachmentReference;

fn view(recalled: Option<Arc<RecalledEntries>>) -> BriefView {
    BriefView::new(
        vec![
            (
                "m-1".to_owned(),
                "We ship on Friday after the review.".to_owned(),
            ),
            ("m-2".to_owned(), "Use the staging cluster.".to_owned()),
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
    assert!(view.check(&excerpt("m-1", "ship on Friday")).is_ok());
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

/// A memory entry must have come from `recall` in this turn.
#[test]
fn a_memory_entry_must_have_been_recalled() {
    let brief = Brief {
        memory_entries: vec!["7".to_owned()],
        ..Brief::default()
    };
    assert_eq!(view(None).check(&brief), Err(BRIEF_MEMORY_NOT_RECALLED));
    let recalled = Arc::new(RecalledEntries::default());
    assert_eq!(
        view(Some(Arc::clone(&recalled))).check(&brief),
        Err(BRIEF_MEMORY_NOT_RECALLED)
    );
    recalled.record_for_test("7", "the team prefers short reports");
    let checked = view(Some(recalled)).check(&brief).unwrap();
    assert_eq!(
        checked.memory_entries,
        [("7".to_owned(), "the team prefers short reports".to_owned())]
    );
}
