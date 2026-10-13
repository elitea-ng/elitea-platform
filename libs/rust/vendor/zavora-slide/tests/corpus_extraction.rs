//! Integration tests for rich extraction (Part K, Req 25).
//!
//! Validates:
//! - `to_outline` on the corpus deck produces correct structure (slides with titles, elements)
//! - `to_markdown` on the corpus deck produces rich output
//! - Round-trip: create deck → extract → verify content matches what was inserted
//! - JSON serialization of `DeckOutline` round-trips correctly
//!
//! Requirements: 25.1, 25.2, 25.3, 26.5

use zavora_slide::{
    Bullet, DeckOutline, Emu, Layout, OutlineElement, Presentation, to_markdown, to_outline,
};

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

// ---------------------------------------------------------------------------
// to_outline on corpus deck
// ---------------------------------------------------------------------------

#[test]
fn corpus_outline_has_correct_slide_count() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    assert_eq!(
        outline.slides.len(),
        p.slide_count(),
        "outline slide count must match presentation slide count"
    );
}

#[test]
fn corpus_outline_slides_numbered_sequentially() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    for (i, slide) in outline.slides.iter().enumerate() {
        assert_eq!(
            slide.number,
            i + 1,
            "slide numbers must be 1-based sequential"
        );
    }
}

#[test]
fn corpus_outline_first_slide_has_content() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    // The first slide should have either a title or body elements containing
    // the deck's title text ("Corpus Sample"). For opened decks, the build model
    // may represent all text as body paragraphs.
    let has_title = outline.slides[0].title.is_some();
    let has_title_in_elements = outline.slides[0].elements.iter().any(|e| match e {
        OutlineElement::Paragraph { text, .. } => text.contains("Corpus Sample"),
        OutlineElement::ShapeText { text, .. } => text.contains("Corpus Sample"),
        _ => false,
    });
    assert!(
        has_title || has_title_in_elements,
        "first slide should have title text either as title or in elements"
    );
}

#[test]
fn corpus_outline_has_body_paragraphs() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    // At least one slide should have paragraph elements
    let has_paragraphs = outline.slides.iter().any(|s| {
        s.elements
            .iter()
            .any(|e| matches!(e, OutlineElement::Paragraph { .. }))
    });
    assert!(has_paragraphs, "corpus deck should have body paragraphs");
}

#[test]
fn corpus_outline_has_text_content() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    // The corpus deck has a free text box — for opened decks this may appear
    // as a body paragraph or as ShapeText depending on how the build model
    // was populated. Verify the text is present somewhere in the outline.
    let has_free_text = outline.slides.iter().any(|s| {
        s.elements.iter().any(|e| match e {
            OutlineElement::Paragraph { text, .. } => text.contains("Free text box"),
            OutlineElement::ShapeText { text, .. } => text.contains("Free text box"),
            _ => false,
        })
    });
    assert!(
        has_free_text,
        "corpus deck should have 'Free text box' content in outline"
    );
}

#[test]
fn corpus_outline_elements_have_content() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    // Every element should have non-empty content
    for slide in &outline.slides {
        for element in &slide.elements {
            match element {
                OutlineElement::Paragraph { text, .. } => {
                    assert!(!text.is_empty(), "paragraph text must not be empty");
                }
                OutlineElement::ShapeText { text, .. } => {
                    assert!(!text.is_empty(), "shape text must not be empty");
                }
                OutlineElement::Table { rows, cols, .. } => {
                    assert!(*rows > 0 && *cols > 0, "table must have dimensions");
                }
                OutlineElement::AltText { text } => {
                    assert!(!text.is_empty(), "alt text must not be empty");
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// to_markdown on corpus deck — rich output
// ---------------------------------------------------------------------------

#[test]
fn corpus_markdown_has_structured_headers() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    // Every slide gets a ## header
    for i in 1..=p.slide_count() {
        assert!(
            md.contains(&format!("## Slide {}", i)),
            "markdown must have header for slide {i}"
        );
    }
}

#[test]
fn corpus_markdown_has_bullet_formatting() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    // Bullets are formatted with "- " prefix
    assert!(
        md.contains("- "),
        "markdown should contain bullet-formatted lines"
    );
}

#[test]
fn corpus_markdown_preserves_all_text_content() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    let md = to_markdown(&p);

    // Every paragraph and shape text from the outline must appear in the markdown
    for slide in &outline.slides {
        if let Some(title) = &slide.title {
            assert!(
                md.contains(title.as_str()),
                "title '{}' must appear in markdown",
                title
            );
        }
        for element in &slide.elements {
            match element {
                OutlineElement::Paragraph { text, .. } => {
                    assert!(
                        md.contains(text.as_str()),
                        "paragraph '{}' must appear in markdown",
                        text
                    );
                }
                OutlineElement::ShapeText { text, .. } => {
                    assert!(
                        md.contains(text.as_str()),
                        "shape text '{}' must appear in markdown",
                        text
                    );
                }
                _ => {}
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Round-trip: create deck → extract → verify content matches
// ---------------------------------------------------------------------------

#[test]
fn roundtrip_title_and_bullets_extracted_correctly() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Round-Trip Title").unwrap();
        s.add_bullets(&[
            Bullet::new("Alpha"),
            Bullet {
                text: "Beta".into(),
                level: 1,
                bold: false,
            },
            Bullet::new("Gamma"),
        ])
        .unwrap();
    }

    let outline = to_outline(&p);
    assert_eq!(outline.slides.len(), 1);
    assert_eq!(outline.slides[0].title.as_deref(), Some("Round-Trip Title"));
    assert_eq!(outline.slides[0].elements.len(), 3);
    assert_eq!(
        outline.slides[0].elements[0],
        OutlineElement::Paragraph {
            text: "Alpha".into(),
            level: 0
        }
    );
    assert_eq!(
        outline.slides[0].elements[1],
        OutlineElement::Paragraph {
            text: "Beta".into(),
            level: 1
        }
    );
    assert_eq!(
        outline.slides[0].elements[2],
        OutlineElement::Paragraph {
            text: "Gamma".into(),
            level: 0
        }
    );
}

#[test]
fn roundtrip_table_extracted_correctly() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        let tid = s.add_table(2, 2, Emu(0), Emu(0), Emu(5000000), Emu(2000000));
        s.set_table_cell(tid, 0, 0, "H1").unwrap();
        s.set_table_cell(tid, 0, 1, "H2").unwrap();
        s.set_table_cell(tid, 1, 0, "D1").unwrap();
        s.set_table_cell(tid, 1, 1, "D2").unwrap();
    }

    let outline = to_outline(&p);
    let table_el = outline.slides[0]
        .elements
        .iter()
        .find(|e| matches!(e, OutlineElement::Table { .. }));
    assert!(table_el.is_some(), "table should be extracted");
    match table_el.unwrap() {
        OutlineElement::Table { rows, cols, cells } => {
            assert_eq!(*rows, 2);
            assert_eq!(*cols, 2);
            assert_eq!(cells, &["H1", "H2", "D1", "D2"]);
        }
        _ => unreachable!(),
    }
}

#[test]
fn roundtrip_notes_extracted_correctly() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("With Notes").unwrap();
        s.set_notes("These are speaker notes");
    }

    let outline = to_outline(&p);
    assert_eq!(
        outline.slides[0].notes.as_deref(),
        Some("These are speaker notes")
    );
}

#[test]
fn roundtrip_textbox_extracted_as_shape_text() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.add_text_box(
            "Annotation",
            Emu(100000),
            Emu(100000),
            Emu(2000000),
            Emu(500000),
        );
    }

    let outline = to_outline(&p);
    let shape_el = outline.slides[0]
        .elements
        .iter()
        .find(|e| matches!(e, OutlineElement::ShapeText { .. }));
    assert!(
        shape_el.is_some(),
        "text box should be extracted as ShapeText"
    );
    match shape_el.unwrap() {
        OutlineElement::ShapeText { kind, text } => {
            assert_eq!(kind, "textbox");
            assert_eq!(text, "Annotation");
        }
        _ => unreachable!(),
    }
}

#[test]
fn roundtrip_multiple_slides_extracted_in_order() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    p.add_slide(Layout::TitleContent);
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("First").unwrap();
    }
    {
        let mut s = p.slide_mut(1).unwrap();
        s.set_title("Second").unwrap();
    }
    {
        let mut s = p.slide_mut(2).unwrap();
        s.set_title("Third").unwrap();
    }

    let outline = to_outline(&p);
    assert_eq!(outline.slides.len(), 3);
    assert_eq!(outline.slides[0].title.as_deref(), Some("First"));
    assert_eq!(outline.slides[1].title.as_deref(), Some("Second"));
    assert_eq!(outline.slides[2].title.as_deref(), Some("Third"));
}

#[test]
fn roundtrip_markdown_contains_all_inserted_content() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Intro Slide").unwrap();
        s.add_bullets(&[Bullet::new("Point A"), Bullet::new("Point B")])
            .unwrap();
        s.set_notes("Remember this");
    }
    {
        let mut s = p.slide_mut(1).unwrap();
        s.set_title("Data Slide").unwrap();
        let tid = s.add_table(2, 2, Emu(0), Emu(0), Emu(4000000), Emu(2000000));
        s.set_table_cell(tid, 0, 0, "Col1").unwrap();
        s.set_table_cell(tid, 0, 1, "Col2").unwrap();
        s.set_table_cell(tid, 1, 0, "Val1").unwrap();
        s.set_table_cell(tid, 1, 1, "Val2").unwrap();
    }

    let md = to_markdown(&p);
    // Titles
    assert!(md.contains("## Slide 1: Intro Slide"));
    assert!(md.contains("## Slide 2: Data Slide"));
    // Bullets
    assert!(md.contains("- Point A"));
    assert!(md.contains("- Point B"));
    // Notes
    assert!(md.contains("> **Note:** Remember this"));
    // Table
    assert!(md.contains("| Col1 | Col2 |"));
    assert!(md.contains("| Val1 | Val2 |"));
    // Separator
    assert!(md.contains("---"));
}

// ---------------------------------------------------------------------------
// JSON serialization round-trip of DeckOutline
// ---------------------------------------------------------------------------

#[test]
fn json_serialization_roundtrip_empty_deck() {
    let p = Presentation::new();
    let outline = to_outline(&p);
    let json = serde_json::to_string(&outline).unwrap();
    let parsed: DeckOutline = serde_json::from_str(&json).unwrap();
    assert_eq!(parsed, outline);
}

#[test]
fn json_serialization_roundtrip_rich_deck() {
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("JSON Title").unwrap();
        s.add_bullets(&[
            Bullet::new("Item 1"),
            Bullet {
                text: "Sub-item".into(),
                level: 1,
                bold: false,
            },
        ])
        .unwrap();
        s.set_notes("Note text");
        s.add_text_box("Box text", Emu(0), Emu(0), Emu(2000000), Emu(500000));
    }
    {
        let mut s = p.slide_mut(1).unwrap();
        s.set_title("Table Slide").unwrap();
        let tid = s.add_table(2, 2, Emu(0), Emu(0), Emu(4000000), Emu(2000000));
        s.set_table_cell(tid, 0, 0, "A").unwrap();
        s.set_table_cell(tid, 0, 1, "B").unwrap();
        s.set_table_cell(tid, 1, 0, "C").unwrap();
        s.set_table_cell(tid, 1, 1, "D").unwrap();
    }

    let outline = to_outline(&p);
    let json = serde_json::to_string_pretty(&outline).unwrap();

    // Verify JSON contains expected fields
    assert!(json.contains("\"JSON Title\""));
    assert!(json.contains("\"Item 1\""));
    assert!(json.contains("\"Sub-item\""));
    assert!(json.contains("\"Note text\""));
    assert!(json.contains("\"Box text\""));
    assert!(json.contains("\"Table\""));

    // Round-trip: deserialize and compare
    let parsed: DeckOutline = serde_json::from_str(&json).unwrap();
    assert_eq!(
        parsed, outline,
        "JSON round-trip must produce identical DeckOutline"
    );
}

#[test]
fn json_serialization_roundtrip_corpus_deck() {
    let p = Presentation::open(SAMPLE).unwrap();
    let outline = to_outline(&p);
    let json = serde_json::to_string(&outline).unwrap();
    let parsed: DeckOutline = serde_json::from_str(&json).unwrap();
    assert_eq!(
        parsed, outline,
        "corpus deck outline must survive JSON round-trip"
    );
}

#[test]
fn json_outline_has_tagged_element_types() {
    // Verify the serde tag-based serialization produces expected type discriminators
    let mut p = Presentation::new();
    p.add_slide(Layout::TitleContent);
    {
        let mut s = p.slide_mut(0).unwrap();
        s.set_title("Tagged").unwrap();
        s.add_bullets(&[Bullet::new("Bullet")]).unwrap();
        s.add_text_box("Shape", Emu(0), Emu(0), Emu(2000000), Emu(500000));
    }

    let outline = to_outline(&p);
    let json = serde_json::to_string(&outline).unwrap();

    // The serde(tag = "type") attribute should produce "type":"Paragraph" etc.
    assert!(
        json.contains("\"type\":\"Paragraph\""),
        "JSON should have tagged Paragraph type"
    );
    assert!(
        json.contains("\"type\":\"ShapeText\""),
        "JSON should have tagged ShapeText type"
    );
}
