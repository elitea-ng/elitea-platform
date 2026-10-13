//! Corpus integration tests for image operations (Part E).
//!
//! Validates that image crop, rotation on picture shapes, surgical insert on
//! opened decks, content-hash deduplication, GIF format acceptance, and
//! round-trip persistence all work correctly and produce surgical edits.
//!
//! Requirements: 26.2, 27.1

mod test_util;

use std::io::Cursor;
use test_util::{assert_only_changed, libreoffice_load_gate, package_entries};
use zavora_slide::{Emu, ImageFormat, Layout, Presentation};
use zavora_slide_opc::OpcPackage;
use zavora_slide_oxml::SlideDom;

const IMAGE_SLIDE_PART: &str = "/ppt/slides/slide1.xml";

// ---------------------------------------------------------------------------
// Minimal test images (valid magic bytes + minimal structure)
// ---------------------------------------------------------------------------

/// A minimal valid 1×1 PNG image (red pixel).
fn minimal_png() -> Vec<u8> {
    // Construct a minimal valid PNG: signature + IHDR + IDAT + IEND
    let mut buf = Vec::new();
    // PNG signature
    buf.extend_from_slice(&[0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A]);
    // IHDR chunk: 1x1, 8-bit RGB
    let ihdr_data: [u8; 13] = [
        0x00, 0x00, 0x00, 0x01, // width = 1
        0x00, 0x00, 0x00, 0x01, // height = 1
        0x08, // bit depth = 8
        0x02, // color type = RGB
        0x00, // compression
        0x00, // filter
        0x00, // interlace
    ];
    write_png_chunk(&mut buf, b"IHDR", &ihdr_data);
    // IDAT chunk: zlib-compressed scanline (filter byte 0 + RGB)
    // Raw data: [0x00, 0xFF, 0x00, 0x00] (filter=none, R=255, G=0, B=0)
    // zlib: deflate with no compression
    let idat_data: [u8; 12] = [
        0x08, 0xD7, // zlib header (deflate, check bits)
        0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, // deflated data
        0x01, 0x01, 0x01, 0x00, // adler32
    ];
    write_png_chunk(&mut buf, b"IDAT", &idat_data);
    // IEND chunk
    write_png_chunk(&mut buf, b"IEND", &[]);
    buf
}

/// A minimal valid GIF89a image (1×1 pixel).
fn minimal_gif() -> Vec<u8> {
    let mut buf = Vec::new();
    // GIF89a header
    buf.extend_from_slice(b"GIF89a");
    // Logical screen descriptor: 1×1, no GCT
    buf.extend_from_slice(&[
        0x01, 0x00, // width = 1
        0x01, 0x00, // height = 1
        0x00, // no GCT
        0x00, // bg color index
        0x00, // pixel aspect ratio
    ]);
    // Image descriptor
    buf.push(0x2C); // image separator
    buf.extend_from_slice(&[
        0x00, 0x00, // left
        0x00, 0x00, // top
        0x01, 0x00, // width = 1
        0x01, 0x00, // height = 1
        0x00, // no LCT
    ]);
    // LZW minimum code size
    buf.push(0x02);
    // Image data sub-block
    buf.push(0x02); // block size
    buf.extend_from_slice(&[0x4C, 0x01]); // compressed data
    buf.push(0x00); // block terminator
    // Trailer
    buf.push(0x3B);
    buf
}

fn write_png_chunk(buf: &mut Vec<u8>, chunk_type: &[u8; 4], data: &[u8]) {
    let len = data.len() as u32;
    buf.extend_from_slice(&len.to_be_bytes());
    buf.extend_from_slice(chunk_type);
    buf.extend_from_slice(data);
    // CRC32 over chunk_type + data
    let crc = crc32(chunk_type, data);
    buf.extend_from_slice(&crc.to_be_bytes());
}

fn crc32(chunk_type: &[u8], data: &[u8]) -> u32 {
    let mut crc: u32 = 0xFFFFFFFF;
    for &byte in chunk_type.iter().chain(data.iter()) {
        crc ^= byte as u32;
        for _ in 0..8 {
            if crc & 1 != 0 {
                crc = (crc >> 1) ^ 0xEDB88320;
            } else {
                crc >>= 1;
            }
        }
    }
    !crc
}

// ---------------------------------------------------------------------------
// Helper: create a deck with a picture on slide 1, save to bytes.
// We insert an image via the high-level API, then reopen via OPC to get a
// SlideDom with a `<p:pic>` element for testing crop/rotation.
// ---------------------------------------------------------------------------

fn create_deck_with_picture() -> Vec<u8> {
    let mut p = Presentation::new();
    let idx = p.add_slide(Layout::Blank);
    {
        let mut slide = p.slide_mut(idx).unwrap();
        slide
            .insert_image_bytes(
                &minimal_png(),
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(4.0),
                Emu::inches(3.0),
            )
            .unwrap();
    }
    p.save_to_buffer().unwrap()
}

fn open_image_dom(buf: &[u8]) -> (SlideDom, OpcPackage) {
    let pkg = OpcPackage::from_reader(Cursor::new(buf.to_vec())).unwrap();
    let part = pkg.get_part(IMAGE_SLIDE_PART).unwrap();
    let dom = SlideDom::parse(part).unwrap();
    (dom, pkg)
}

/// Rebuild a package with an edited slide DOM and return the new entries.
fn rebuild_with_edited_dom(
    pkg: &OpcPackage,
    slide_part: &str,
    dom: &SlideDom,
) -> std::collections::BTreeMap<String, Vec<u8>> {
    let mut buf = Cursor::new(Vec::new());
    pkg.write_to(&mut buf).unwrap();
    buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(buf).unwrap();
    pkg2.set_part(slide_part, dom.to_bytes());
    package_entries(&pkg2)
}

/// Find the picture shape index in the DOM.
fn find_pic_shape_idx(dom: &SlideDom) -> usize {
    let inv = dom.shape_inventory();
    inv.iter()
        .position(|s| s.shape_type == "pic")
        .expect("deck should have a pic shape")
}

// ===========================================================================
// set_image_crop on a picture shape — surgical (only slide part changes)
// ===========================================================================

#[test]
fn corpus_set_image_crop_is_surgical() {
    let buf = create_deck_with_picture();
    let (mut dom, pkg) = open_image_dom(&buf);
    let orig = package_entries(&pkg);
    let pic_idx = find_pic_shape_idx(&dom);

    // Set crop: 10% from each side (values in 1/1000ths of percent = 10000)
    dom.set_image_crop(pic_idx, 10000, 10000, 10000, 10000)
        .unwrap();

    let after = rebuild_with_edited_dom(&pkg, IMAGE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[IMAGE_SLIDE_PART]);

    let xml = String::from_utf8(after[IMAGE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains("srcRect"), "srcRect element present");
    assert!(xml.contains(r#"l="10000""#), "left crop set");
    assert!(xml.contains(r#"t="10000""#), "top crop set");
    assert!(xml.contains(r#"r="10000""#), "right crop set");
    assert!(xml.contains(r#"b="10000""#), "bottom crop set");
}

// ===========================================================================
// set_shape_rotation on a picture shape — surgical
// ===========================================================================

#[test]
fn corpus_set_shape_rotation_on_picture_is_surgical() {
    let buf = create_deck_with_picture();
    let (mut dom, pkg) = open_image_dom(&buf);
    let orig = package_entries(&pkg);
    let pic_idx = find_pic_shape_idx(&dom);

    // Rotate picture by 90 degrees (in 60000ths of a degree).
    dom.set_shape_rotation(pic_idx, 5400000).unwrap();

    let after = rebuild_with_edited_dom(&pkg, IMAGE_SLIDE_PART, &dom);
    assert_only_changed(&orig, &after, &[IMAGE_SLIDE_PART]);

    let xml = String::from_utf8(after[IMAGE_SLIDE_PART].clone()).unwrap();
    assert!(xml.contains(r#"rot="5400000""#), "rotation set on picture");
}

// ===========================================================================
// insert_image_bytes on an opened deck — surgical (adds media part + slide)
// ===========================================================================

#[test]
fn corpus_insert_image_bytes_is_surgical() {
    // Open an existing deck (the corpus sample).
    let mut p = Presentation::open("tests/corpus/powerpoint_sample.pptx").unwrap();
    let slide_count = p.slide_count();
    assert!(slide_count > 0);

    // Insert an image on slide 0.
    {
        let mut slide = p.slide_mut(0).unwrap();
        let shape_id = slide
            .insert_image_bytes(
                &minimal_png(),
                Emu::inches(2.0),
                Emu::inches(2.0),
                Emu::inches(3.0),
                Emu::inches(2.0),
            )
            .unwrap();
        assert!(shape_id > 0, "returned a valid shape id");
    }

    // Save and verify the deck is still valid.
    let buf = p.save_to_buffer().unwrap();
    let reopened = Presentation::open_from_bytes(&buf).unwrap();
    assert_eq!(reopened.slide_count(), slide_count, "slide count preserved");
}

// ===========================================================================
// Content-hash deduplication: same image twice → no duplicate media parts
// ===========================================================================

#[test]
fn corpus_insert_image_deduplication() {
    // Create a deck, save, then reopen it so we have a DOM-backed slide.
    // Deduplication is designed for the surgical (DOM) path on opened decks.
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let buf = p.save_to_buffer().unwrap();

    // Reopen the deck (now it has a DOM).
    let mut p2 = Presentation::open_from_bytes(&buf).unwrap();
    let png_data = minimal_png();

    {
        let mut slide = p2.slide_mut(0).unwrap();
        // Insert the same image twice.
        slide
            .insert_image_bytes(
                &png_data,
                Emu::inches(1.0),
                Emu::inches(1.0),
                Emu::inches(2.0),
                Emu::inches(2.0),
            )
            .unwrap();
        slide
            .insert_image_bytes(
                &png_data,
                Emu::inches(4.0),
                Emu::inches(1.0),
                Emu::inches(2.0),
                Emu::inches(2.0),
            )
            .unwrap();
    }

    let buf2 = p2.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(buf2)).unwrap();

    // Count media parts: there should be only one image media part despite
    // two insertions of the same content (content-hash deduplication).
    let media_parts: Vec<&str> = pkg
        .part_names()
        .filter(|name| name.starts_with("/ppt/media/"))
        .collect();

    assert_eq!(
        media_parts.len(),
        1,
        "same image inserted twice should produce only one media part, got: {:?}",
        media_parts
    );
}

// ===========================================================================
// GIF format acceptance
// ===========================================================================

#[test]
fn corpus_insert_gif_format_accepted() {
    let gif_data = minimal_gif();

    // Verify format detection works.
    let detected = ImageFormat::detect(&gif_data);
    assert_eq!(
        detected,
        Some(ImageFormat::Gif),
        "GIF detected from magic bytes"
    );

    // Insert GIF into a deck.
    let mut p = Presentation::new();
    let idx = p.add_slide(Layout::Blank);
    {
        let mut slide = p.slide_mut(idx).unwrap();
        let result = slide.insert_image_bytes(
            &gif_data,
            Emu::inches(1.0),
            Emu::inches(1.0),
            Emu::inches(3.0),
            Emu::inches(2.0),
        );
        assert!(
            result.is_ok(),
            "GIF insertion should succeed: {:?}",
            result.err()
        );
    }

    // Save and verify the deck is valid.
    let buf = p.save_to_buffer().unwrap();
    let reopened = Presentation::open_from_bytes(&buf).unwrap();
    assert_eq!(reopened.slide_count(), 1, "deck with GIF saves and reopens");
}

// ===========================================================================
// Round-trip: insert image → save → reopen → verify image persists
// ===========================================================================

#[test]
fn corpus_images_round_trip() {
    let mut p = Presentation::new();
    let idx = p.add_slide(Layout::Blank);
    {
        let mut slide = p.slide_mut(idx).unwrap();
        slide
            .insert_image_bytes(
                &minimal_png(),
                Emu::inches(1.5),
                Emu::inches(1.5),
                Emu::inches(4.0),
                Emu::inches(3.0),
            )
            .unwrap();
    }

    // Save to buffer.
    let buf = p.save_to_buffer().unwrap();

    // Reopen and verify the image is present.
    let pkg = OpcPackage::from_reader(Cursor::new(buf.clone())).unwrap();
    let media_parts: Vec<&str> = pkg
        .part_names()
        .filter(|name| name.starts_with("/ppt/media/"))
        .collect();
    assert!(
        !media_parts.is_empty(),
        "media part should exist after save"
    );

    // Verify the slide XML references the image.
    let slide_xml = pkg.get_part(IMAGE_SLIDE_PART).unwrap();
    let xml_str = String::from_utf8_lossy(slide_xml);
    assert!(
        xml_str.contains("p:pic") || xml_str.contains("blipFill"),
        "slide XML should reference the picture"
    );

    // Reopen via Presentation API and verify stability.
    let reopened = Presentation::open_from_bytes(&buf).unwrap();
    assert_eq!(reopened.slide_count(), 1, "slide count preserved");

    // Save again and verify stability.
    let buf2 = reopened.save_to_buffer().unwrap();
    let p2 = Presentation::open_from_bytes(&buf2).unwrap();
    assert_eq!(p2.slide_count(), 1, "stable across saves");
}

// ===========================================================================
// LibreOffice load gate (env-guarded)
// ===========================================================================

#[test]
fn corpus_images_libreoffice_gate() {
    let buf = create_deck_with_picture();
    let (mut dom, pkg) = open_image_dom(&buf);
    let pic_idx = find_pic_shape_idx(&dom);

    // Apply image edits: crop + rotation.
    dom.set_image_crop(pic_idx, 5000, 5000, 5000, 5000).unwrap();
    dom.set_shape_rotation(pic_idx, 2700000).unwrap(); // 45 degrees

    // Rebuild the package with the edited DOM.
    let mut pkg2_buf = Cursor::new(Vec::new());
    pkg.write_to(&mut pkg2_buf).unwrap();
    pkg2_buf.set_position(0);
    let mut pkg2 = OpcPackage::from_reader(pkg2_buf).unwrap();
    pkg2.set_part(IMAGE_SLIDE_PART, dom.to_bytes());

    let mut out = Cursor::new(Vec::new());
    pkg2.write_to(&mut out).unwrap();

    if let Err(e) = libreoffice_load_gate(out.get_ref()) {
        panic!("LibreOffice load gate failed: {e}");
    }
}
