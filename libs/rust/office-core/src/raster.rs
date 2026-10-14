//! Rasterise an SVG the caller built (ADR-0033 decision 6).
//!
//! `data_analysis` draws its charts as SVG text from a table. This turns that
//! text into a PNG with `resvg`, the rasteriser the pptx renderer already
//! links, and the bundled Liberation Sans only: no system font is read, so
//! the picture is the same on every host. The SVG is the caller's own, not an
//! upload: raster images and external references are off, so a stray `href`
//! in a label cannot fetch or decode anything.

use resvg::{tiny_skia, usvg};

use crate::error::{Limit, OfficeError, Result, limit};
use crate::guard::run_guarded;
use crate::limits::Limits;

const REGULAR: &[u8] =
    include_bytes!("../../vendor/zavora-slide-render/fonts/LiberationSans-Regular.ttf");
const BOLD: &[u8] =
    include_bytes!("../../vendor/zavora-slide-render/fonts/LiberationSans-Bold.ttf");

/// Draw `svg` as a PNG of at most `max_pixels` pixels, on the dedicated parse
/// thread (so it holds a document permit like every other call).
///
/// # Errors
/// [`Limit::RenderPixels`] when the picture is larger than `max_pixels`,
/// [`OfficeError::InvalidInput`] for SVG that does not parse,
/// [`OfficeError::Busy`] and [`OfficeError::DeadlineExceeded`] as for any call.
pub fn svg_to_png(svg: &str, max_pixels: u64, limits: &Limits) -> Result<Vec<u8>> {
    if svg.len() > 8 * 1024 * 1024 {
        return Err(limit(Limit::InputBytes, svg.len() as u64));
    }
    let svg = svg.to_owned();
    run_guarded(limits, move || {
        let mut options = usvg::Options {
            image_href_resolver: usvg::ImageHrefResolver {
                // Neither a data: image nor a file path is followed.
                resolve_data: Box::new(|_, _, _| None),
                resolve_string: Box::new(|_, _| None),
            },
            ..usvg::Options::default()
        };
        let fonts = options.fontdb_mut();
        fonts.load_font_data(REGULAR.to_vec());
        fonts.load_font_data(BOLD.to_vec());
        fonts.set_sans_serif_family("Liberation Sans");
        let tree = usvg::Tree::from_str(&svg, &options)
            .map_err(|e| OfficeError::InvalidInput(format!("svg: {e}")))?;
        let size = tree.size().to_int_size();
        let pixels = u64::from(size.width()) * u64::from(size.height());
        if pixels == 0 || pixels > max_pixels {
            return Err(limit(Limit::RenderPixels, pixels));
        }
        let mut pixmap = tiny_skia::Pixmap::new(size.width(), size.height())
            .ok_or_else(|| OfficeError::Internal("cannot allocate the picture".into()))?;
        resvg::render(
            &tree,
            tiny_skia::Transform::identity(),
            &mut pixmap.as_mut(),
        );
        pixmap
            .encode_png()
            .map_err(|e| OfficeError::Internal(format!("png: {e}")))
    })
}
