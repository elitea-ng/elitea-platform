//! PDF export: one page per slide. Each slide's [`Scene`] is converted to SVG,
//! embedded as a vector XObject via `svg2pdf`, and scaled to fill a page sized
//! to the slide's EMU dimensions (1 pt = 12700 EMU).

use std::collections::HashMap;

use pdf_writer::{Content, Finish, Name, Pdf, Rect, Ref};
use svg2pdf::usvg;
use zavora_slide_layout::Scene;

const EMU_PER_POINT: f64 = 12700.0;
/// SVG raster width used as the conversion basis (vector output, not pixels).
const SVG_W: u32 = 1280;

#[derive(Debug, thiserror::Error)]
pub enum PdfError {
    #[error("SVG parse failed: {0}")]
    Svg(String),
    #[error("SVG→PDF conversion failed")]
    Convert,
}

/// Build a multi-page PDF (one page per scene) and return the bytes.
pub fn scenes_to_pdf(scenes: &[Scene]) -> Result<Vec<u8>, PdfError> {
    let mut alloc = Ref::new(1);
    let catalog_id = alloc.bump();
    let page_tree_id = alloc.bump();

    let mut pdf = Pdf::new();
    let mut page_ids = Vec::new();
    // Deferred writes: (page_id, content_id, svg_name, svg_ref, w_pt, h_pt).
    struct PagePlan {
        page_id: Ref,
        content_id: Ref,
        svg_ref: Ref,
        w_pt: f32,
        h_pt: f32,
    }
    let mut plans = Vec::new();

    for scene in scenes {
        let w_pt = (scene.width_emu as f64 / EMU_PER_POINT) as f32;
        let h_pt = (scene.height_emu as f64 / EMU_PER_POINT) as f32;

        let svg = zavora_slide_render::scene_to_svg(scene, SVG_W);
        let mut opt = usvg::Options::default();
        let db = opt.fontdb_mut();
        // Elitea patch (libs/rust/vendor/README.md): bundled fonts only.
        #[cfg(feature = "bundled-fonts")]
        {
            db.load_font_data(include_bytes!("../fonts/LiberationSans-Regular.ttf").to_vec());
            db.load_font_data(include_bytes!("../fonts/LiberationSans-Bold.ttf").to_vec());
            db.set_sans_serif_family("Liberation Sans");
        }
        let tree = usvg::Tree::from_str(&svg, &opt).map_err(|e| PdfError::Svg(e.to_string()))?;
        let (chunk, root) = svg2pdf::to_chunk(&tree, svg2pdf::ConversionOptions::default())
            .map_err(|_| PdfError::Convert)?;

        // Renumber the SVG chunk into our id space.
        let mut map = HashMap::new();
        let chunk = chunk.renumber(|old| *map.entry(old).or_insert_with(|| alloc.bump()));
        let svg_ref = *map.get(&root).ok_or(PdfError::Convert)?;
        pdf.extend(&chunk);

        let page_id = alloc.bump();
        let content_id = alloc.bump();
        page_ids.push(page_id);
        plans.push(PagePlan {
            page_id,
            content_id,
            svg_ref,
            w_pt,
            h_pt,
        });
    }

    pdf.catalog(catalog_id).pages(page_tree_id);
    pdf.pages(page_tree_id)
        .kids(page_ids.iter().copied())
        .count(page_ids.len() as i32);

    let svg_name = Name(b"S1");
    for p in &plans {
        let mut page = pdf.page(p.page_id);
        page.media_box(Rect::new(0.0, 0.0, p.w_pt, p.h_pt));
        page.parent(page_tree_id);
        page.contents(p.content_id);
        page.resources().x_objects().pair(svg_name, p.svg_ref);
        page.finish();

        // The XObject is 1pt×1pt; scale it to the full page.
        let mut content = Content::new();
        content
            .transform([p.w_pt, 0.0, 0.0, p.h_pt, 0.0, 0.0])
            .x_object(svg_name);
        pdf.stream(p.content_id, &content.finish());
    }

    Ok(pdf.finish())
}

#[cfg(test)]
mod tests {
    use super::*;
    use zavora_slide_layout::{Color, Item, Rect as LRect, Scene, TextFrameProps, TextLine};

    fn scene() -> Scene {
        let mut s = Scene::new(12192000, 6858000);
        s.items.push(Item::Text {
            rect: LRect {
                x: 914400,
                y: 457200,
                w: 9000000,
                h: 914400,
            },
            lines: vec![TextLine {
                text: "Page".into(),
                size_pt: 32.0,
                color: Color::BLACK,
                bold: true,
                italic: false,
                level: 0,
                is_paragraph_start: true,
                ..TextLine::default()
            }],
            props: TextFrameProps::default(),
        });
        s
    }

    #[test]
    fn multi_page_pdf() {
        let pdf = scenes_to_pdf(&[scene(), scene()]).unwrap();
        assert_eq!(&pdf[0..5], b"%PDF-");
        // Two pages → "/Count 2" appears in the page tree.
        let txt = String::from_utf8_lossy(&pdf);
        assert!(txt.contains("/Count 2"));
    }
}
