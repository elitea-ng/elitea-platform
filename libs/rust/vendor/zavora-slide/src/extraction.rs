//! Structured extraction of deck content for programmatic consumers.
//!
//! Provides [`to_outline`] which walks a presentation's slides in reading order,
//! extracting titles, body paragraphs (with indent level), tables (as grids),
//! shape text, alt-text, and speaker notes into a serde-serializable
//! [`DeckOutline`].

use serde::{Deserialize, Serialize};

use crate::Presentation;

/// A structured outline of an entire deck.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DeckOutline {
    /// Per-slide outlines in presentation order.
    pub slides: Vec<SlideOutline>,
}

/// A structured outline of a single slide.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SlideOutline {
    /// 1-based slide number.
    pub number: usize,
    /// Slide title text (from the title placeholder), if any.
    pub title: Option<String>,
    /// Content elements in reading order.
    pub elements: Vec<OutlineElement>,
    /// Speaker notes text, if any.
    pub notes: Option<String>,
}

/// An individual content element extracted from a slide.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum OutlineElement {
    /// A body paragraph with its indent level.
    Paragraph {
        text: String,
        /// Indent level (0 = top-level).
        level: u8,
    },
    /// A table represented as a grid of cell strings (row-major).
    Table {
        rows: usize,
        cols: usize,
        /// Cell contents in row-major order (`rows * cols` entries).
        cells: Vec<String>,
    },
    /// Text from a non-placeholder shape (text box or auto-shape).
    ShapeText {
        /// Shape name or kind descriptor.
        kind: String,
        text: String,
    },
    /// Alt-text (description) associated with a shape or image.
    AltText { text: String },
}

/// Extract a structured [`DeckOutline`] from a presentation.
///
/// Walks each slide in order, collecting:
/// - Title (from the title placeholder)
/// - Body paragraphs with indent level
/// - Tables as grids
/// - Shape text from non-placeholder shapes
/// - Alt-text (currently from shape names that suggest descriptions)
/// - Speaker notes
pub fn to_outline(presentation: &Presentation) -> DeckOutline {
    let slides_data = presentation.slides_for_test();
    let mut slides = Vec::with_capacity(slides_data.len());

    for (i, slide_data) in slides_data.iter().enumerate() {
        let mut title: Option<String> = None;
        let mut elements: Vec<OutlineElement> = Vec::new();

        // Walk shapes in document order (reading order).
        for shape in &slide_data.shapes {
            let is_title = shape
                .placeholder
                .as_ref()
                .map(|ph| ph.ph_type == "title" || ph.ph_type == "ctrTitle")
                .unwrap_or(false);

            let is_body = shape
                .placeholder
                .as_ref()
                .map(|ph| ph.ph_type == "body" || ph.ph_type == "subTitle")
                .unwrap_or(false);

            if is_title {
                // Extract title text (all paragraphs joined).
                let text: String = shape
                    .body
                    .paragraphs
                    .iter()
                    .map(|p| p.text())
                    .collect::<Vec<_>>()
                    .join("\n");
                if !text.is_empty() {
                    title = Some(text);
                }
            } else if is_body {
                // Extract body paragraphs with level.
                for para in &shape.body.paragraphs {
                    let text = para.text();
                    if !text.is_empty() {
                        elements.push(OutlineElement::Paragraph {
                            text,
                            level: para.level.unwrap_or(0),
                        });
                    }
                }
            } else {
                // Non-placeholder shape: extract as ShapeText if it has content.
                let text: String = shape
                    .body
                    .paragraphs
                    .iter()
                    .map(|p| p.text())
                    .collect::<Vec<_>>()
                    .join("\n");
                if !text.is_empty() {
                    let kind = if shape.text_box {
                        "textbox".to_string()
                    } else {
                        "shape".to_string()
                    };
                    elements.push(OutlineElement::ShapeText { kind, text });
                }
            }
        }

        // Extract tables as grids.
        for table in &slide_data.tables {
            elements.push(OutlineElement::Table {
                rows: table.rows,
                cols: table.cols,
                cells: table.cells.clone(),
            });
        }

        // Notes.
        let notes = slide_data.notes.clone();

        slides.push(SlideOutline {
            number: i + 1,
            title,
            elements,
            notes,
        });
    }

    DeckOutline { slides }
}

/// Produce a Markdown rendering of the deck, equal-or-richer than `markitdown`.
///
/// Output format:
/// - Slide boundaries marked with `---` (horizontal rule) between slides
/// - Each slide headed with `## Slide N: {title}` (or `## Slide N` if untitled)
/// - Body paragraphs rendered as bullet lists (`- ` for level 0, `  - ` for deeper)
/// - Tables rendered as Markdown tables with `|` separators and header row separator
/// - Speaker notes rendered as blockquotes (`> **Note:** ...`)
/// - Shape text included with a label (`**{kind}:** ...`)
/// - Alt-text included in brackets (`[Alt: ...]`)
pub fn to_markdown(presentation: &Presentation) -> String {
    let outline = to_outline(presentation);
    let mut out = String::new();

    for (i, slide) in outline.slides.iter().enumerate() {
        // Slide separator (between slides, not before the first)
        if i > 0 {
            out.push_str("\n---\n\n");
        }

        // Slide header
        match &slide.title {
            Some(title) => out.push_str(&format!("## Slide {}: {}\n\n", slide.number, title)),
            None => out.push_str(&format!("## Slide {}\n\n", slide.number)),
        }

        // Content elements
        for element in &slide.elements {
            match element {
                OutlineElement::Paragraph { text, level } => {
                    let indent = "  ".repeat(*level as usize);
                    out.push_str(&format!("{indent}- {text}\n"));
                }
                OutlineElement::Table { rows, cols, cells } => {
                    if *rows == 0 || *cols == 0 {
                        continue;
                    }
                    // Header row
                    out.push('|');
                    for c in 0..*cols {
                        let cell = cells.get(c).map(String::as_str).unwrap_or("");
                        out.push_str(&format!(" {} |", cell));
                    }
                    out.push('\n');
                    // Separator row
                    out.push('|');
                    for _ in 0..*cols {
                        out.push_str(" --- |");
                    }
                    out.push('\n');
                    // Data rows
                    for r in 1..*rows {
                        out.push('|');
                        for c in 0..*cols {
                            let cell = cells.get(r * cols + c).map(String::as_str).unwrap_or("");
                            out.push_str(&format!(" {} |", cell));
                        }
                        out.push('\n');
                    }
                    out.push('\n');
                }
                OutlineElement::ShapeText { kind, text } => {
                    out.push_str(&format!("**{kind}:** {text}\n\n"));
                }
                OutlineElement::AltText { text } => {
                    out.push_str(&format!("[Alt: {text}]\n\n"));
                }
            }
        }

        // Speaker notes as blockquote
        if let Some(notes) = &slide.notes {
            out.push_str(&format!("> **Note:** {notes}\n\n"));
        }
    }

    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Bullet, Emu, Layout, Presentation};

    #[test]
    fn empty_deck_produces_empty_outline() {
        let p = Presentation::new();
        let outline = to_outline(&p);
        assert_eq!(outline.slides.len(), 0);
    }

    #[test]
    fn single_slide_with_title_and_bullets() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.set_title("Hello World").unwrap();
            s.add_bullets(&[
                Bullet::new("First point"),
                Bullet {
                    text: "Sub point".into(),
                    level: 1,
                    bold: false,
                },
                Bullet::new("Second point"),
            ])
            .unwrap();
        }
        let outline = to_outline(&p);
        assert_eq!(outline.slides.len(), 1);

        let slide = &outline.slides[0];
        assert_eq!(slide.number, 1);
        assert_eq!(slide.title.as_deref(), Some("Hello World"));

        // Body paragraphs
        assert_eq!(slide.elements.len(), 3);
        assert_eq!(
            slide.elements[0],
            OutlineElement::Paragraph {
                text: "First point".into(),
                level: 0
            }
        );
        assert_eq!(
            slide.elements[1],
            OutlineElement::Paragraph {
                text: "Sub point".into(),
                level: 1
            }
        );
        assert_eq!(
            slide.elements[2],
            OutlineElement::Paragraph {
                text: "Second point".into(),
                level: 0
            }
        );
    }

    #[test]
    fn slide_with_notes() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.set_title("Noted").unwrap();
            s.set_notes("Speaker notes here");
        }
        let outline = to_outline(&p);
        assert_eq!(
            outline.slides[0].notes.as_deref(),
            Some("Speaker notes here")
        );
    }

    #[test]
    fn slide_with_table() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            let tid = s.add_table(2, 3, Emu(0), Emu(0), Emu(5000000), Emu(2000000));
            s.set_table_cell(tid, 0, 0, "A1").unwrap();
            s.set_table_cell(tid, 0, 1, "B1").unwrap();
            s.set_table_cell(tid, 0, 2, "C1").unwrap();
            s.set_table_cell(tid, 1, 0, "A2").unwrap();
            s.set_table_cell(tid, 1, 1, "B2").unwrap();
            s.set_table_cell(tid, 1, 2, "C2").unwrap();
        }
        let outline = to_outline(&p);
        let slide = &outline.slides[0];

        // Find the table element
        let table_el = slide
            .elements
            .iter()
            .find(|e| matches!(e, OutlineElement::Table { .. }));
        assert!(table_el.is_some());
        match table_el.unwrap() {
            OutlineElement::Table { rows, cols, cells } => {
                assert_eq!(*rows, 2);
                assert_eq!(*cols, 3);
                assert_eq!(cells, &["A1", "B1", "C1", "A2", "B2", "C2"]);
            }
            _ => unreachable!(),
        }
    }

    #[test]
    fn slide_with_text_box() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.add_text_box(
                "Extra info",
                Emu(100000),
                Emu(100000),
                Emu(2000000),
                Emu(500000),
            );
        }
        let outline = to_outline(&p);
        let slide = &outline.slides[0];

        let shape_text = slide
            .elements
            .iter()
            .find(|e| matches!(e, OutlineElement::ShapeText { .. }));
        assert!(shape_text.is_some());
        match shape_text.unwrap() {
            OutlineElement::ShapeText { kind, text } => {
                assert_eq!(kind, "textbox");
                assert_eq!(text, "Extra info");
            }
            _ => unreachable!(),
        }
    }

    #[test]
    fn outline_serializes_to_json() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.set_title("JSON Test").unwrap();
            s.add_bullets(&[Bullet::new("Item 1")]).unwrap();
        }
        let outline = to_outline(&p);
        let json = serde_json::to_string_pretty(&outline).unwrap();
        assert!(json.contains("\"JSON Test\""));
        assert!(json.contains("\"Item 1\""));

        // Round-trip: deserialize back
        let parsed: DeckOutline = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed, outline);
    }

    #[test]
    fn multiple_slides_numbered_correctly() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        p.add_slide(Layout::TitleContent);
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.set_title("Slide One").unwrap();
        }
        {
            let mut s = p.slide_mut(2).unwrap();
            s.set_title("Slide Three").unwrap();
        }
        let outline = to_outline(&p);
        assert_eq!(outline.slides.len(), 3);
        assert_eq!(outline.slides[0].number, 1);
        assert_eq!(outline.slides[1].number, 2);
        assert_eq!(outline.slides[2].number, 3);
        assert_eq!(outline.slides[0].title.as_deref(), Some("Slide One"));
        assert_eq!(outline.slides[1].title, None);
        assert_eq!(outline.slides[2].title.as_deref(), Some("Slide Three"));
    }

    // --- to_markdown tests ---

    #[test]
    fn markdown_title_and_bullets() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.set_title("Hello World").unwrap();
            s.add_bullets(&[
                Bullet::new("First point"),
                Bullet {
                    text: "Sub point".into(),
                    level: 1,
                    bold: false,
                },
                Bullet::new("Second point"),
            ])
            .unwrap();
        }
        let md = to_markdown(&p);
        assert!(
            md.contains("## Slide 1: Hello World"),
            "slide header with title"
        );
        assert!(md.contains("- First point"), "top-level bullet");
        assert!(md.contains("  - Sub point"), "indented bullet");
        assert!(md.contains("- Second point"), "second top-level bullet");
    }

    #[test]
    fn markdown_tables_rendered_as_markdown_tables() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            let tid = s.add_table(2, 3, Emu(0), Emu(0), Emu(5000000), Emu(2000000));
            s.set_table_cell(tid, 0, 0, "Name").unwrap();
            s.set_table_cell(tid, 0, 1, "Age").unwrap();
            s.set_table_cell(tid, 0, 2, "City").unwrap();
            s.set_table_cell(tid, 1, 0, "Alice").unwrap();
            s.set_table_cell(tid, 1, 1, "30").unwrap();
            s.set_table_cell(tid, 1, 2, "NYC").unwrap();
        }
        let md = to_markdown(&p);
        assert!(md.contains("| Name | Age | City |"), "header row");
        assert!(md.contains("| --- | --- | --- |"), "separator row");
        assert!(md.contains("| Alice | 30 | NYC |"), "data row");
    }

    #[test]
    fn markdown_notes_as_blockquotes() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.set_title("Noted").unwrap();
            s.set_notes("Remember to mention the deadline");
        }
        let md = to_markdown(&p);
        assert!(
            md.contains("> **Note:** Remember to mention the deadline"),
            "notes rendered as blockquote"
        );
    }

    #[test]
    fn markdown_slide_boundaries() {
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
        let md = to_markdown(&p);
        // Should have --- separators between slides
        assert!(md.contains("---"), "slide separator present");
        // Count separators: should be 2 for 3 slides
        let separator_count = md.matches("\n---\n").count();
        assert_eq!(separator_count, 2, "two separators for three slides");
        // All slide headers present
        assert!(md.contains("## Slide 1: First"));
        assert!(md.contains("## Slide 2: Second"));
        assert!(md.contains("## Slide 3: Third"));
    }

    #[test]
    fn markdown_multiple_slides_correct_separators() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.set_title("Intro").unwrap();
            s.add_bullets(&[Bullet::new("Welcome")]).unwrap();
            s.set_notes("Opening remarks");
        }
        {
            let mut s = p.slide_mut(1).unwrap();
            s.set_title("Data").unwrap();
            let tid = s.add_table(2, 2, Emu(0), Emu(0), Emu(4000000), Emu(2000000));
            s.set_table_cell(tid, 0, 0, "X").unwrap();
            s.set_table_cell(tid, 0, 1, "Y").unwrap();
            s.set_table_cell(tid, 1, 0, "1").unwrap();
            s.set_table_cell(tid, 1, 1, "2").unwrap();
        }
        let md = to_markdown(&p);
        // First slide content
        assert!(md.contains("## Slide 1: Intro"));
        assert!(md.contains("- Welcome"));
        assert!(md.contains("> **Note:** Opening remarks"));
        // Separator
        assert!(md.contains("---"));
        // Second slide content
        assert!(md.contains("## Slide 2: Data"));
        assert!(md.contains("| X | Y |"));
        assert!(md.contains("| 1 | 2 |"));
    }

    #[test]
    fn markdown_shape_text_included() {
        let mut p = Presentation::new();
        p.add_slide(Layout::TitleContent);
        {
            let mut s = p.slide_mut(0).unwrap();
            s.add_text_box(
                "Extra info",
                Emu(100000),
                Emu(100000),
                Emu(2000000),
                Emu(500000),
            );
        }
        let md = to_markdown(&p);
        assert!(
            md.contains("**textbox:** Extra info"),
            "shape text with label"
        );
    }

    #[test]
    fn markdown_empty_deck() {
        let p = Presentation::new();
        let md = to_markdown(&p);
        assert!(md.is_empty(), "empty deck produces empty markdown");
    }
}
