//! LibreOffice similarity gate test (env-guarded).
//!
//! Validates Requirement 18 (Render parity check):
//! - 18.1: Compare our PNG against a LibreOffice-rendered PNG, reporting similarity.
//! - 18.2: A curated set of slides meets a documented minimum similarity threshold.
//!
//! This test is **env-guarded**: it only runs the LibreOffice comparison when
//! `ZAVORA_LIBREOFFICE_GATE=1`. When the gate is not set, the test passes trivially
//! (the comparison is skipped).
//!
//! ## Threshold rationale
//!
//! The initial threshold is set to 0.5 (50% similarity). This is intentionally
//! conservative — our renderer uses different fonts, simplified geometry, and
//! estimated text layout compared to LibreOffice. As render fidelity improves
//! (geometry inheritance, real text shaping, theme resolution), this threshold
//! should be tightened toward 0.8+.
//!
//! ## Running the gate
//!
//! ```sh
//! ZAVORA_LIBREOFFICE_GATE=1 cargo test --package zavora-slide similarity_gate
//! ```

mod test_util;

use test_util::similarity::{SIMILARITY_THRESHOLD, compare_pngs, render_with_libreoffice};
use zavora_slide::{Presentation, RenderFormat};

const SAMPLE: &str = "tests/corpus/powerpoint_sample.pptx";

/// Integration test: render slide 0 with our engine and LibreOffice, compare.
///
/// When `ZAVORA_LIBREOFFICE_GATE=1`:
/// - Renders slide 0 with our engine (via `render_slide` → `scene_to_png`).
/// - Renders slide 0 with LibreOffice headless.
/// - Computes similarity score.
/// - Asserts score >= SIMILARITY_THRESHOLD.
///
/// When the gate is not set, the test passes (skipped).
#[test]
fn libreoffice_similarity_gate_slide_0() {
    let p = Presentation::open(SAMPLE).expect("open corpus deck");

    // Render with our engine.
    let our_png = p
        .render_slide(0, RenderFormat::Png)
        .expect("our engine renders slide 0");

    // Render with LibreOffice (env-guarded).
    let pptx_bytes = std::fs::read(SAMPLE).expect("read corpus pptx");
    let lo_result = render_with_libreoffice(&pptx_bytes, 0);

    match lo_result {
        None => {
            // Gate not set — test is a no-op.
            eprintln!(
                "ZAVORA_LIBREOFFICE_GATE not set; skipping LibreOffice similarity check. \
                 Set ZAVORA_LIBREOFFICE_GATE=1 to enable."
            );
        }
        Some(Ok(lo_png)) => {
            let score = compare_pngs(&our_png, &lo_png);
            eprintln!("Similarity score (slide 0): {score:.4} (threshold: {SIMILARITY_THRESHOLD})");
            assert!(
                score >= SIMILARITY_THRESHOLD,
                "Similarity score {score:.4} is below threshold {SIMILARITY_THRESHOLD}. \
                 Our render has regressed relative to LibreOffice."
            );
        }
        Some(Err(e)) => {
            eprintln!("LibreOffice render failed (non-fatal in CI without LO): {e}");
            // If the gate is set but LO fails, that's a real error.
            panic!("LibreOffice render failed with ZAVORA_LIBREOFFICE_GATE=1: {e}");
        }
    }
}

// --- Unit tests for compare_pngs ---

#[test]
fn compare_identical_pngs_returns_one() {
    // Create a simple 2x2 solid-color PNG via tiny_skia.
    let mut pixmap = resvg::tiny_skia::Pixmap::new(2, 2).unwrap();
    // Fill with a solid color.
    pixmap.fill(resvg::tiny_skia::Color::from_rgba8(100, 150, 200, 255));
    let png = pixmap.encode_png().unwrap();

    let score = compare_pngs(&png, &png);
    assert!(
        (score - 1.0).abs() < 1e-10,
        "identical images should score 1.0, got {score}"
    );
}

#[test]
fn compare_very_different_pngs_below_half() {
    // Create two 4x4 PNGs with opposite colors.
    let mut white = resvg::tiny_skia::Pixmap::new(4, 4).unwrap();
    white.fill(resvg::tiny_skia::Color::from_rgba8(255, 255, 255, 255));
    let white_png = white.encode_png().unwrap();

    let mut black = resvg::tiny_skia::Pixmap::new(4, 4).unwrap();
    black.fill(resvg::tiny_skia::Color::from_rgba8(0, 0, 0, 255));
    let black_png = black.encode_png().unwrap();

    let score = compare_pngs(&white_png, &black_png);
    assert!(
        score < 0.5,
        "very different images should score below 0.5, got {score}"
    );
}

#[test]
fn compare_similar_pngs_above_threshold() {
    // Create two 4x4 PNGs with slightly different colors.
    let mut a = resvg::tiny_skia::Pixmap::new(4, 4).unwrap();
    a.fill(resvg::tiny_skia::Color::from_rgba8(100, 100, 100, 255));
    let a_png = a.encode_png().unwrap();

    let mut b = resvg::tiny_skia::Pixmap::new(4, 4).unwrap();
    b.fill(resvg::tiny_skia::Color::from_rgba8(110, 105, 95, 255));
    let b_png = b.encode_png().unwrap();

    let score = compare_pngs(&a_png, &b_png);
    assert!(
        score > 0.9,
        "similar images should score above 0.9, got {score}"
    );
}

#[test]
fn compare_different_size_pngs_penalizes() {
    // A 4x4 vs 8x8 image — overlap is only 25% of the larger area.
    let mut small = resvg::tiny_skia::Pixmap::new(4, 4).unwrap();
    small.fill(resvg::tiny_skia::Color::from_rgba8(128, 128, 128, 255));
    let small_png = small.encode_png().unwrap();

    let mut large = resvg::tiny_skia::Pixmap::new(8, 8).unwrap();
    large.fill(resvg::tiny_skia::Color::from_rgba8(128, 128, 128, 255));
    let large_png = large.encode_png().unwrap();

    let score = compare_pngs(&small_png, &large_png);
    // Overlap is 4*4=16 out of 8*8=64 → coverage = 0.25.
    // Colors are identical in overlap → MAE = 0 → (1-0)*0.25 = 0.25.
    assert!(
        (score - 0.25).abs() < 0.01,
        "size-mismatched identical-color images should score ~0.25, got {score}"
    );
}
