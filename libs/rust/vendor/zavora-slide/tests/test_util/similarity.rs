//! PNG similarity comparison utilities for render fidelity testing.
//!
//! Provides:
//! - `compare_pngs`: pixel-by-pixel mean absolute error comparison (0.0–1.0).
//! - `render_with_libreoffice`: env-guarded LibreOffice headless PNG render.
//!
//! The similarity gate is env-guarded: it only runs when `ZAVORA_LIBREOFFICE_GATE=1`.

/// Minimum similarity threshold for the LibreOffice comparison gate.
///
/// This is intentionally set low (0.5) as a baseline. As render fidelity improves,
/// this threshold should be tightened. A score of 1.0 means identical; 0.0 means
/// completely different.
pub const SIMILARITY_THRESHOLD: f64 = 0.5;

/// Compare two PNG images and return a similarity score in [0.0, 1.0].
///
/// Uses mean absolute error (MAE) across all pixel channels, normalized so that:
/// - 1.0 = identical images
/// - 0.0 = maximally different (every channel at opposite extremes)
///
/// Both images are decoded via `resvg::tiny_skia::Pixmap::decode_png`. If the images
/// have different dimensions, the score is computed over the overlapping region and
/// penalized proportionally for the size mismatch.
///
/// # Panics
///
/// Panics if either PNG cannot be decoded.
pub fn compare_pngs(our_png: &[u8], reference_png: &[u8]) -> f64 {
    let our_pixmap =
        resvg::tiny_skia::Pixmap::decode_png(our_png).expect("failed to decode our PNG");
    let ref_pixmap = resvg::tiny_skia::Pixmap::decode_png(reference_png)
        .expect("failed to decode reference PNG");

    let our_w = our_pixmap.width() as usize;
    let our_h = our_pixmap.height() as usize;
    let ref_w = ref_pixmap.width() as usize;
    let ref_h = ref_pixmap.height() as usize;

    // Compute overlap region.
    let overlap_w = our_w.min(ref_w);
    let overlap_h = our_h.min(ref_h);

    if overlap_w == 0 || overlap_h == 0 {
        return 0.0;
    }

    let our_data = our_pixmap.data();
    let ref_data = ref_pixmap.data();

    // Compute MAE over the overlapping pixels (4 channels: RGBA premultiplied).
    let mut total_diff: u64 = 0;
    let overlap_pixels = overlap_w * overlap_h;

    for y in 0..overlap_h {
        for x in 0..overlap_w {
            let our_idx = (y * our_w + x) * 4;
            let ref_idx = (y * ref_w + x) * 4;

            for c in 0..4 {
                let ours = our_data[our_idx + c] as i32;
                let theirs = ref_data[ref_idx + c] as i32;
                total_diff += (ours - theirs).unsigned_abs() as u64;
            }
        }
    }

    // Normalize: max possible diff per pixel is 255 * 4 channels.
    let max_diff = overlap_pixels as u64 * 255 * 4;
    let mae = total_diff as f64 / max_diff as f64;

    // Penalize for size mismatch: the score covers only the overlap fraction.
    let max_area = (our_w * our_h).max(ref_w * ref_h) as f64;
    let overlap_area = (overlap_w * overlap_h) as f64;
    let coverage = overlap_area / max_area;

    // Final score: (1 - MAE) * coverage
    (1.0 - mae) * coverage
}

/// Render a specific slide from a `.pptx` file to PNG using LibreOffice headless.
///
/// This function is **env-guarded**: it only runs when `ZAVORA_LIBREOFFICE_GATE=1`.
/// When the variable is unset or set to any other value, returns `None` (skipped).
///
/// # Arguments
///
/// * `pptx_bytes` - The raw bytes of the `.pptx` file.
/// * `slide_index` - The 0-based index of the slide to render (note: LibreOffice
///   exports all slides; we pick the one at the given index).
///
/// # Returns
///
/// - `None` if the env gate is not set (test should be skipped).
/// - `Some(Ok(png_bytes))` on success.
/// - `Some(Err(message))` on failure.
pub fn render_with_libreoffice(
    pptx_bytes: &[u8],
    slide_index: usize,
) -> Option<Result<Vec<u8>, String>> {
    let gate = std::env::var("ZAVORA_LIBREOFFICE_GATE").unwrap_or_default();
    if gate != "1" {
        return None;
    }

    Some(render_with_libreoffice_inner(pptx_bytes, slide_index))
}

fn render_with_libreoffice_inner(pptx_bytes: &[u8], slide_index: usize) -> Result<Vec<u8>, String> {
    use std::fs;
    use std::process::Command;

    // Write pptx to a temp file.
    let tmp_dir = std::env::temp_dir().join("zavora_similarity_gate");
    fs::create_dir_all(&tmp_dir).map_err(|e| format!("failed to create temp dir: {e}"))?;

    let pptx_path = tmp_dir.join("test_deck.pptx");
    fs::write(&pptx_path, pptx_bytes).map_err(|e| format!("failed to write temp pptx: {e}"))?;

    let out_dir = tmp_dir.join("png_output");
    fs::create_dir_all(&out_dir).map_err(|e| format!("failed to create output dir: {e}"))?;

    // Run LibreOffice headless to convert to PNG.
    // LibreOffice exports one PNG per slide when converting from pptx.
    let result = Command::new("libreoffice")
        .args(["--headless", "--convert-to", "png", "--outdir"])
        .arg(&out_dir)
        .arg(&pptx_path)
        .output();

    let output = match result {
        Ok(o) => o,
        Err(e) => {
            // Clean up
            let _ = fs::remove_dir_all(&tmp_dir);
            return Err(format!("failed to run libreoffice: {e}"));
        }
    };

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        let _ = fs::remove_dir_all(&tmp_dir);
        return Err(format!(
            "libreoffice exited with {}: {}",
            output.status, stderr
        ));
    }

    // LibreOffice produces a single PNG named after the input file (e.g. test_deck.png)
    // when converting pptx to png. For multi-slide decks, it may produce only the
    // first slide or all slides depending on version. We look for the output file.
    let png_path = out_dir.join("test_deck.png");

    let png_bytes = if png_path.exists() {
        // Single-file output (common for pptx→png conversion).
        // If slide_index > 0, we cannot extract individual slides this way.
        if slide_index > 0 {
            let _ = fs::remove_dir_all(&tmp_dir);
            return Err(format!(
                "LibreOffice single-PNG export only supports slide 0; requested index {slide_index}"
            ));
        }
        fs::read(&png_path).map_err(|e| format!("failed to read output PNG: {e}"))?
    } else {
        // Look for numbered output (e.g. test_deck-0.png, test_deck-1.png, etc.)
        // or Impress-style naming.
        let mut candidates: Vec<_> = fs::read_dir(&out_dir)
            .map_err(|e| format!("failed to read output dir: {e}"))?
            .filter_map(|entry| entry.ok())
            .filter(|entry| entry.path().extension().is_some_and(|ext| ext == "png"))
            .collect();

        candidates.sort_by_key(|e| e.file_name());

        if candidates.is_empty() {
            let _ = fs::remove_dir_all(&tmp_dir);
            return Err("LibreOffice produced no PNG output".into());
        }

        let target = candidates.get(slide_index).ok_or_else(|| {
            format!(
                "LibreOffice produced {} PNG(s) but slide index {slide_index} requested",
                candidates.len()
            )
        })?;

        fs::read(target.path()).map_err(|e| format!("failed to read output PNG: {e}"))?
    };

    // Clean up.
    let _ = fs::remove_dir_all(&tmp_dir);

    Ok(png_bytes)
}
