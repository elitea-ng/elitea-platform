//! Draw every slide of a deck to a PNG, so what the code produces can be looked at.
//!
//! Counting items proves something was drawn; only looking at it proves the right thing was.

fn main() {
    let Some(path) = std::env::args().nth(1) else {
        eprintln!("give it a deck to draw");
        return;
    };
    let deck = match zavora_slide::Presentation::open(&path) {
        Ok(deck) => deck,
        Err(trouble) => {
            eprintln!("could not open {path}: {trouble}");
            return;
        }
    };
    for at in 0..deck.slide_count() {
        let scene = deck.slide(at).unwrap().scene();
        match zavora_slide_render::scene_to_png(&scene, 1000) {
            Ok(png) => {
                let out = format!("/tmp/slide{}.png", at + 1);
                std::fs::write(&out, &png).unwrap();
                println!("{out} — {} items", scene.items.len());
            }
            Err(trouble) => eprintln!("slide {}: {trouble}", at + 1),
        }
    }
}
