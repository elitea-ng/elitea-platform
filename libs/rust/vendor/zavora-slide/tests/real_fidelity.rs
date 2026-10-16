//! What a slide drawn from a real deck contains.
//!
//! Opening a presentation used to keep the slide's XML for editing and, for drawing, gather every
//! run of text into one body placeholder — so a real deck drew as one block of text per slide, a
//! slide whose content was a picture drew as nothing, and no shape had the colour or the position
//! the file gave it. These hold the shape tree being read instead.

use zavora_slide::Presentation;

/// A deck with several shapes on a slide, built the way PowerPoint builds one.
fn a_deck(path: &str) {
    use zavora_slide::{Emu, Layout};

    let _ = std::fs::remove_file(path);
    let mut deck = Presentation::new();
    let index = deck.add_slide(Layout::Blank);
    {
        let mut slide = deck.slide_mut(index).unwrap();
        // Two boxes, as a real slide has: something at the top and something under it.
        slide.add_text_box(
            "The Problem",
            Emu(914_400),
            Emu(457_200),
            Emu(6_400_800),
            Emu(900_000),
        );
        slide.add_text_box(
            "Too much to do",
            Emu(914_400),
            Emu(1_800_000),
            Emu(6_400_800),
            Emu(900_000),
        );
    }
    deck.save(path).unwrap();
}

#[test]
fn a_slide_draws_more_than_one_thing() {
    let path = "/tmp/zavora-fidelity-shapes.pptx";
    a_deck(path);

    let deck = Presentation::open(path).expect("opens");
    let scene = deck.slide(0).expect("a slide").scene();
    assert!(
        scene.items.len() >= 2,
        "a slide with a title and a body drew as {} thing(s)",
        scene.items.len()
    );

    let text: Vec<String> = scene
        .items
        .iter()
        .filter_map(|item| match item {
            zavora_slide::Item::Text { lines, .. } => Some(
                lines
                    .iter()
                    .map(|l| l.text.clone())
                    .collect::<Vec<_>>()
                    .join(" | "),
            ),
            _ => None,
        })
        .collect();
    assert!(
        text.iter().any(|t| t.contains("The Problem")),
        "the title is missing: {text:?}"
    );
    assert!(
        text.iter().any(|t| t.contains("Too much to do")),
        "the body is missing: {text:?}"
    );
}

/// Each drawn thing has to sit somewhere, or it cannot be drawn at all.
#[test]
fn everything_drawn_has_a_place_on_the_slide() {
    let path = "/tmp/zavora-fidelity-places.pptx";
    a_deck(path);

    let deck = Presentation::open(path).expect("opens");
    let slide = deck.slide(0).expect("a slide");
    let scene = slide.scene();

    for (index, item) in scene.items.iter().enumerate() {
        let rect = match item {
            zavora_slide::Item::Text { rect, .. }
            | zavora_slide::Item::Rect { rect, .. }
            | zavora_slide::Item::Image { rect, .. } => *rect,
            _ => continue,
        };
        assert!(
            rect.w > 0 && rect.h > 0,
            "item {index} has no size: {rect:?}"
        );
        assert!(
            rect.x >= 0 && rect.y >= 0,
            "item {index} is off the slide: {rect:?}"
        );
    }
}
