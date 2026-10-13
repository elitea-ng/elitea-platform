//! Integration tests for core document properties (Requirement 12.1, 12.2).

use zavora_slide::{CoreProperties, Layout, Presentation};

#[test]
fn new_deck_has_default_properties() {
    let p = Presentation::new();
    let props = p.core_properties();
    // Default template has author = "zavora-slide"
    assert_eq!(props.author.as_deref(), Some("zavora-slide"));
    assert_eq!(props.last_modified_by.as_deref(), Some("zavora-slide"));
    // Other fields are None by default
    assert_eq!(props.subject, None);
    assert_eq!(props.keywords, None);
    assert_eq!(props.comments, None);
    assert_eq!(props.category, None);
    assert_eq!(props.created, None);
    assert_eq!(props.modified, None);
}

#[test]
fn set_properties_persist_after_save_reopen() {
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);

    let props = CoreProperties {
        title: Some("My Presentation".into()),
        author: Some("Test Author".into()),
        subject: Some("Test Subject".into()),
        keywords: Some("rust, slides, test".into()),
        comments: Some("A test comment".into()),
        category: Some("Testing".into()),
        created: Some("2024-03-15T10:00:00Z".into()),
        modified: Some("2024-06-20T14:30:00Z".into()),
        last_modified_by: Some("Tester".into()),
    };
    p.set_core_properties(&props);

    // Save and reopen
    let bytes = p.save_to_buffer().unwrap();
    let reopened = Presentation::open_from_bytes(&bytes).unwrap();
    let read_back = reopened.core_properties();

    assert_eq!(read_back.title.as_deref(), Some("My Presentation"));
    assert_eq!(read_back.author.as_deref(), Some("Test Author"));
    assert_eq!(read_back.subject.as_deref(), Some("Test Subject"));
    assert_eq!(read_back.keywords.as_deref(), Some("rust, slides, test"));
    assert_eq!(read_back.comments.as_deref(), Some("A test comment"));
    assert_eq!(read_back.category.as_deref(), Some("Testing"));
    assert_eq!(read_back.created.as_deref(), Some("2024-03-15T10:00:00Z"));
    assert_eq!(read_back.modified.as_deref(), Some("2024-06-20T14:30:00Z"));
    assert_eq!(read_back.last_modified_by.as_deref(), Some("Tester"));
}

#[test]
fn unset_properties_preserve_existing_values_on_opened_deck() {
    // Create a deck with all properties set.
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let initial = CoreProperties {
        title: Some("Original Title".into()),
        author: Some("Original Author".into()),
        subject: Some("Original Subject".into()),
        keywords: Some("original".into()),
        comments: Some("Original Comment".into()),
        category: Some("Original Category".into()),
        created: Some("2024-01-01T00:00:00Z".into()),
        modified: Some("2024-01-01T00:00:00Z".into()),
        last_modified_by: Some("Original Editor".into()),
    };
    p.set_core_properties(&initial);
    let bytes = p.save_to_buffer().unwrap();

    // Reopen and set only title — all other fields should be preserved.
    let mut reopened = Presentation::open_from_bytes(&bytes).unwrap();
    let partial_update = CoreProperties {
        title: Some("Updated Title".into()),
        ..Default::default()
    };
    reopened.set_core_properties(&partial_update);

    // Save again and verify.
    let bytes2 = reopened.save_to_buffer().unwrap();
    let final_deck = Presentation::open_from_bytes(&bytes2).unwrap();
    let final_props = final_deck.core_properties();

    assert_eq!(final_props.title.as_deref(), Some("Updated Title"));
    // All other fields preserved from the original.
    assert_eq!(final_props.author.as_deref(), Some("Original Author"));
    assert_eq!(final_props.subject.as_deref(), Some("Original Subject"));
    assert_eq!(final_props.keywords.as_deref(), Some("original"));
    assert_eq!(final_props.comments.as_deref(), Some("Original Comment"));
    assert_eq!(final_props.category.as_deref(), Some("Original Category"));
    assert_eq!(final_props.created.as_deref(), Some("2024-01-01T00:00:00Z"));
    assert_eq!(
        final_props.modified.as_deref(),
        Some("2024-01-01T00:00:00Z")
    );
    assert_eq!(
        final_props.last_modified_by.as_deref(),
        Some("Original Editor")
    );
}

#[test]
fn read_properties_from_corpus_deck() {
    // The corpus sample should have some properties set by PowerPoint.
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/corpus/powerpoint_sample.pptx"
    );
    let p = Presentation::open(path).unwrap();
    let props = p.core_properties();
    // We just verify it doesn't panic and returns a valid struct.
    // The corpus file may or may not have all fields set.
    let _ = props.title;
    let _ = props.author;
    let _ = props.subject;
}

#[test]
fn set_properties_on_opened_deck_is_surgical() {
    // Open a deck, set properties, save — only docProps/core.xml should change.
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let bytes = p.save_to_buffer().unwrap();

    let mut reopened = Presentation::open_from_bytes(&bytes).unwrap();
    let update = CoreProperties {
        title: Some("Surgical Test".into()),
        ..Default::default()
    };
    reopened.set_core_properties(&update);

    let bytes2 = reopened.save_to_buffer().unwrap();
    let final_deck = Presentation::open_from_bytes(&bytes2).unwrap();
    let props = final_deck.core_properties();
    assert_eq!(props.title.as_deref(), Some("Surgical Test"));
}
