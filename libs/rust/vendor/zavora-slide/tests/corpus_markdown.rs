//! Corpus benchmark tests for Markdown extraction (Part K, Req 25.2).
//!
//! Validates that `to_markdown` produces rich Markdown from the corpus deck
//! that is equal-or-richer than `markitdown`: tables as Markdown tables, notes
//! called out as blockquotes, slide boundaries marked.
//!
//! Requirements: 25.2, 26.5

use zavora_slide::{Presentation, to_markdown, to_outline};

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

#[test]
fn corpus_markdown_is_non_empty() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    assert!(
        !md.is_empty(),
        "markdown extraction should produce non-empty output"
    );
    // Should have at least some meaningful content length
    assert!(
        md.len() > 50,
        "markdown should have substantial content, got {} bytes",
        md.len()
    );
}

#[test]
fn corpus_markdown_has_slide_boundaries() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    let slide_count = p.slide_count();

    // Each slide should have a header
    for i in 1..=slide_count {
        assert!(
            md.contains(&format!("## Slide {}", i)),
            "missing header for slide {i}"
        );
    }

    // Separators between slides (slide_count - 1 separators)
    if slide_count > 1 {
        let separator_count = md.matches("\n---\n").count();
        assert_eq!(
            separator_count,
            slide_count - 1,
            "expected {} separators for {} slides",
            slide_count - 1,
            slide_count
        );
    }
}

#[test]
fn corpus_markdown_contains_title_text() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    // The corpus deck has "Corpus Sample" as the title of slide 1
    assert!(
        md.contains("Corpus Sample"),
        "title text should be present in markdown"
    );
}

#[test]
fn corpus_markdown_contains_bullet_text() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    // The corpus deck has bullet items
    assert!(
        md.contains("First item"),
        "bullet text should be present in markdown"
    );
}

#[test]
fn corpus_markdown_contains_textbox_content() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    // The corpus deck has a free text box
    assert!(
        md.contains("Free text box"),
        "text box content should be present in markdown"
    );
}

#[test]
fn corpus_markdown_notes_as_blockquotes() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    let outline = to_outline(&p);

    // If any slide has notes, they should appear as blockquotes
    let has_notes = outline.slides.iter().any(|s| s.notes.is_some());
    if has_notes {
        assert!(
            md.contains("> **Note:**"),
            "notes should be rendered as blockquotes"
        );
    }
}

#[test]
fn corpus_markdown_tables_as_markdown_tables() {
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    let outline = to_outline(&p);

    // If any slide has tables, they should appear as Markdown tables
    let has_tables = outline.slides.iter().any(|s| {
        s.elements
            .iter()
            .any(|e| matches!(e, zavora_slide::OutlineElement::Table { .. }))
    });
    if has_tables {
        // Markdown tables use | separators
        assert!(md.contains("|"), "tables should use pipe separators");
        assert!(
            md.contains("---"),
            "tables should have header separator row"
        );
    }
}

#[test]
fn corpus_markdown_richness_benchmark() {
    // Benchmark: the Markdown output should be at least as rich as a simple
    // text dump. We verify it contains structured elements that markitdown
    // would produce: slide headers, bullet formatting, and proper structure.
    let p = Presentation::open(SAMPLE).unwrap();
    let md = to_markdown(&p);
    let outline = to_outline(&p);

    // Count content elements across all slides
    let _total_elements: usize = outline.slides.iter().map(|s| s.elements.len()).sum();

    // The markdown should reference all slides
    assert_eq!(
        md.matches("## Slide").count(),
        outline.slides.len(),
        "all slides should have headers"
    );

    // Every non-empty paragraph from the outline should appear in the markdown
    for slide in &outline.slides {
        for element in &slide.elements {
            match element {
                zavora_slide::OutlineElement::Paragraph { text, .. } => {
                    assert!(
                        md.contains(text.as_str()),
                        "paragraph text '{}' should appear in markdown",
                        text
                    );
                }
                zavora_slide::OutlineElement::ShapeText { text, .. } => {
                    assert!(
                        md.contains(text.as_str()),
                        "shape text '{}' should appear in markdown",
                        text
                    );
                }
                _ => {}
            }
        }
    }

    // The markdown should be richer than a plain concatenation (has formatting)
    let plain_text: String = outline
        .slides
        .iter()
        .flat_map(|s| {
            s.elements.iter().filter_map(|e| match e {
                zavora_slide::OutlineElement::Paragraph { text, .. } => Some(text.as_str()),
                zavora_slide::OutlineElement::ShapeText { text, .. } => Some(text.as_str()),
                _ => None,
            })
        })
        .collect::<Vec<_>>()
        .join("\n");

    assert!(
        md.len() > plain_text.len(),
        "markdown ({} bytes) should be richer than plain text ({} bytes)",
        md.len(),
        plain_text.len()
    );
}
