//! `svg_to_png` (ADR-0033): the rasteriser `data_analysis` draws charts with.
#![allow(clippy::pedantic)]

use elitea_office_core::{Limit, Limits, OfficeError, svg_to_png};

fn limits() -> Limits {
    Limits {
        max_concurrent: 64,
        ..Limits::default()
    }
}

const SVG: &str = r##"<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" viewBox="0 0 200 100"><rect width="200" height="100" fill="#ffffff"/><rect x="10" y="10" width="80" height="80" fill="#4e79a7"/><text x="100" y="50" font-family="Liberation Sans, sans-serif" font-size="20" fill="#222">Hello</text></svg>"##;

fn decode(png: &[u8]) -> (u32, u32, Vec<u8>) {
    let decoder = image::codecs::png::PngDecoder::new(std::io::Cursor::new(png)).expect("png");
    let image = image::DynamicImage::from_decoder(decoder)
        .expect("decode")
        .to_rgba8();
    (image.width(), image.height(), image.into_raw())
}

#[test]
fn draws_a_png_with_the_bundled_font() {
    let png = svg_to_png(SVG, 1_000_000, &limits()).expect("png");
    assert!(png.starts_with(&[0x89, b'P', b'N', b'G']));
    let (w, h, pixels) = decode(&png);
    assert_eq!((w, h), (200, 100));
    // The blue square is drawn, and text pixels exist right of it.
    let at = |x: usize, y: usize| &pixels[(y * 200 + x) * 4..(y * 200 + x) * 4 + 4];
    assert_eq!(at(50, 50), &[0x4e, 0x79, 0xa7, 0xff]);
    let text_pixels = (0..100)
        .flat_map(|y| (100..190).map(move |x| (x, y)))
        .filter(|(x, y)| at(*x, *y)[0] < 0x80)
        .count();
    assert!(
        text_pixels > 50,
        "the label was not drawn ({text_pixels} dark pixels)"
    );
}

#[test]
fn the_same_svg_is_the_same_png() {
    assert_eq!(
        svg_to_png(SVG, 1_000_000, &limits()).unwrap(),
        svg_to_png(SVG, 1_000_000, &limits()).unwrap()
    );
}

#[test]
fn a_picture_over_the_pixel_cap_is_refused() {
    let big = SVG.replace(
        "width=\"200\" height=\"100\"",
        "width=\"4000\" height=\"4000\"",
    );
    match svg_to_png(&big, 1_000_000, &limits()) {
        Err(OfficeError::LimitExceeded { limit, value }) => {
            assert_eq!(limit, Limit::RenderPixels);
            assert_eq!(value, 16_000_000);
        }
        other => panic!("{other:?}"),
    }
}

#[test]
fn bad_svg_and_oversized_text_are_refused() {
    assert!(matches!(
        svg_to_png("<svg", 1_000_000, &limits()),
        Err(OfficeError::InvalidInput(_))
    ));
    let huge = format!(
        "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"1\" height=\"1\">{}</svg>",
        " ".repeat(9 * 1024 * 1024)
    );
    assert!(matches!(
        svg_to_png(&huge, 1_000_000, &limits()),
        Err(OfficeError::LimitExceeded {
            limit: Limit::InputBytes,
            ..
        })
    ));
}

#[test]
fn images_and_files_in_the_svg_are_not_followed() {
    let svg = r#"<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="20" height="20"><image href="file:///etc/hosts" width="20" height="20"/><image xlink:href="data:image/png;base64,iVBORw0KGgo=" width="20" height="20"/></svg>"#;
    let png = svg_to_png(svg, 1_000_000, &limits()).expect("renders, drawing nothing");
    let (_, _, pixels) = decode(&png);
    assert!(pixels.chunks(4).all(|p| p[3] == 0), "nothing was drawn");
}
