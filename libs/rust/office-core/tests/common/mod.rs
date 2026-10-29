//! Shared builders for the integration tests: limits that do not collide on the
//! process-wide permit gate, valid sample files, and zip surgery for hostile ones.
#![allow(dead_code, clippy::pedantic)]

use std::io::{Cursor, Read, Write};

use elitea_office_core::{
    Bullet, CellValue, ChartKind, ChartSpec, Deck, Document, Limits, Rect, Series, SlideLayout,
    Workbook,
};
use zip::write::SimpleFileOptions;
use zip::{CompressionMethod, ZipArchive, ZipWriter};

/// Default limits, with room on the gate for tests that run in parallel.
pub fn limits() -> Limits {
    Limits {
        max_concurrent: 64,
        ..Limits::default()
    }
}

/// A minimal valid PNG header (IHDR only): enough for the header probe, and
/// for media that the libraries never decode.
pub fn png_header(w: u32, h: u32) -> Vec<u8> {
    let mut v = b"\x89PNG\r\n\x1a\n\0\0\0\rIHDR".to_vec();
    v.extend_from_slice(&w.to_be_bytes());
    v.extend_from_slice(&h.to_be_bytes());
    v.extend_from_slice(&[8, 2, 0, 0, 0]);
    v
}

/// A real, tiny, valid 1x1 PNG.
pub const TINY_PNG: &[u8] = &[
    0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
    0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
    0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0xF8, 0xFF, 0xFF, 0x3F,
    0x00, 0x05, 0xFE, 0x02, 0xFE, 0xA7, 0x35, 0x81, 0x84, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
    0x44, 0xAE, 0x42, 0x60, 0x82,
];

pub fn good_deck() -> Vec<u8> {
    let l = limits();
    let mut d = Deck::new(&l);
    let s0 = d.add_slide(SlideLayout::TitleContent).unwrap();
    d.set_title(s0, "{{TITLE}}").unwrap();
    d.add_bullets(s0, &[Bullet::new("{{BULLET}}")]).unwrap();
    d.set_notes(s0, "speaker notes").unwrap();
    let s1 = d.add_slide(SlideLayout::Blank).unwrap();
    d.add_text(s1, "Second {{BODY}}", Rect::inches(1.0, 1.0, 6.0, 1.0))
        .unwrap();
    let t = d
        .add_table(s1, 2, 2, Rect::inches(0.5, 2.5, 6.0, 2.0))
        .unwrap();
    d.set_table_cell(s1, t, 1, 1, "cell").unwrap();
    d.save().unwrap()
}

pub fn good_docx() -> Vec<u8> {
    let l = limits();
    let mut d = Document::new(&l);
    d.add_heading("Hello {{NAME}}", 1).unwrap();
    d.add_paragraph("Body text paragraph.").unwrap();
    let t = d.add_table(2, 2).unwrap();
    d.set_table_cell(t, 0, 0, "a").unwrap();
    d.save().unwrap()
}

pub fn good_xlsx() -> Vec<u8> {
    let l = limits();
    let mut w = Workbook::new(&l);
    w.write_row(
        0,
        0,
        0,
        &[CellValue::Text("Q1".into()), CellValue::Text("Q2".into())],
    )
    .unwrap();
    w.write_row(0, 1, 0, &[CellValue::Number(10.0), CellValue::Number(20.0)])
        .unwrap();
    w.write_cell(
        0,
        2,
        0,
        &CellValue::Formula {
            formula: "SUM(A2:B2)".into(),
            cached: None,
        },
    )
    .unwrap();
    w.save().unwrap()
}

pub fn chart() -> ChartSpec {
    ChartSpec {
        kind: ChartKind::ClusteredColumn,
        categories: vec!["Q1".into(), "Q2".into()],
        series: vec![Series {
            name: "FY".into(),
            values: vec![1.0, 2.0],
        }],
        title: Some("Revenue".into()),
        legend_position: Some("b".into()),
        data_labels: true,
    }
}

/// Entries of a zip, in order.
pub fn entries(bytes: &[u8]) -> Vec<(String, Vec<u8>)> {
    try_entries(bytes).expect("a readable zip")
}

/// Entries of a zip, or `None` when the test zip reader cannot read it.
pub fn try_entries(bytes: &[u8]) -> Option<Vec<(String, Vec<u8>)>> {
    let mut a = ZipArchive::new(Cursor::new(bytes)).ok()?;
    let mut out = Vec::new();
    for i in 0..a.len() {
        let mut f = a.by_index(i).ok()?;
        let mut v = Vec::new();
        f.read_to_end(&mut v).ok()?;
        out.push((f.name().ok()?.into_owned(), v));
    }
    Some(out)
}

/// Write a zip of `(name, data)` entries (deflate).
pub fn zip_of(entries: &[(String, Vec<u8>)]) -> Vec<u8> {
    let mut w = ZipWriter::new(Cursor::new(Vec::new()));
    for (n, d) in entries {
        w.start_file(
            n.as_str(),
            SimpleFileOptions::default().compression_method(CompressionMethod::Deflated),
        )
        .unwrap();
        w.write_all(d).unwrap();
    }
    w.finish().unwrap().into_inner()
}

/// Replace the data of one entry.
pub fn with_entry(bytes: &[u8], name: &str, data: Vec<u8>) -> Vec<u8> {
    let mut e = entries(bytes);
    let slot = e.iter_mut().find(|(n, _)| n == name).expect("entry exists");
    slot.1 = data;
    zip_of(&e)
}

/// Add an entry.
pub fn with_extra(bytes: &[u8], name: &str, data: Vec<u8>) -> Vec<u8> {
    let mut e = entries(bytes);
    e.push((name.to_owned(), data));
    zip_of(&e)
}

pub const MAIN_PPTX: &str = "ppt/slides/slide1.xml";
pub const MAIN_DOCX: &str = "word/document.xml";
pub const MAIN_XLSX: &str = "xl/worksheets/sheet1.xml";

/// A zip bomb: every entry of `good` except `name`, which becomes `mib` MiB of
/// 'A' (deflated, about 1000:1) streamed into the archive.
pub fn bomb(good: &[u8], name: &str, mib: usize) -> Vec<u8> {
    let mut w = ZipWriter::new(Cursor::new(Vec::new()));
    let deflate = SimpleFileOptions::default()
        .compression_method(CompressionMethod::Deflated)
        .compression_level(Some(1));
    for (n, d) in entries(good) {
        if n == name {
            continue;
        }
        w.start_file(n, deflate).unwrap();
        w.write_all(&d).unwrap();
    }
    w.start_file(name, deflate.large_file(true)).unwrap();
    let chunk = vec![b'A'; 1 << 20];
    for _ in 0..mib {
        w.write_all(&chunk).unwrap();
    }
    w.finish().unwrap().into_inner()
}

/// A small (non-zip64) bomb whose headers understate the size: `mib` MiB of
/// 'A' declared as 1000 bytes. The capped reader, not the header, must stop it.
pub fn understated_bomb(good: &[u8], name: &str, mib: usize) -> Vec<u8> {
    let mut w = ZipWriter::new(Cursor::new(Vec::new()));
    let deflate = SimpleFileOptions::default()
        .compression_method(CompressionMethod::Deflated)
        .compression_level(Some(1));
    w.start_file(name, deflate).unwrap();
    let chunk = vec![b'A'; 1 << 20];
    for _ in 0..mib {
        w.write_all(&chunk).unwrap();
    }
    let mut d = w.finish().unwrap().into_inner();
    let pos = |d: &[u8], sig: &[u8]| d.windows(4).position(|w| w == sig).unwrap();
    let (l, c) = (pos(&d, b"PK\x03\x04"), pos(&d, b"PK\x01\x02"));
    d[l + 22..l + 26].copy_from_slice(&1000_u32.to_le_bytes());
    d[c + 24..c + 28].copy_from_slice(&1000_u32.to_le_bytes());
    let _ = good;
    d
}

/// Patch every central-directory and local-header uncompressed size to a lie.
pub fn lie_about_sizes(bytes: &[u8]) -> Vec<u8> {
    let mut d = bytes.to_vec();
    let find = |d: &[u8], sig: &[u8]| -> Vec<usize> {
        d.windows(4)
            .enumerate()
            .filter(|(_, w)| *w == sig)
            .map(|(i, _)| i)
            .collect()
    };
    for o in find(&d, b"PK\x01\x02") {
        d[o + 24..o + 28].copy_from_slice(&0xFFFF_FFF0_u32.to_le_bytes());
    }
    for o in find(&d, b"PK\x03\x04") {
        d[o + 22..o + 26].copy_from_slice(&0xFFFF_FFF0_u32.to_le_bytes());
    }
    d
}

/// `depth` nested elements spliced after the first tag of `main`.
pub fn nested(good: &[u8], main: &str, depth: usize) -> Vec<u8> {
    let x = entries(good)
        .into_iter()
        .find(|(n, _)| n == main)
        .unwrap()
        .1;
    let decl_end = x.windows(2).position(|w| w == b"?>").map_or(0, |i| i + 2);
    let first_open = decl_end + x[decl_end..].iter().position(|&b| b == b'<').unwrap();
    let i = first_open + x[first_open..].iter().position(|&b| b == b'>').unwrap();
    let mut out = x[..=i].to_vec();
    for _ in 0..depth {
        out.extend_from_slice(b"<x:d xmlns:x='urn:x'>");
    }
    for _ in 0..depth {
        out.extend_from_slice(b"</x:d>");
    }
    out.extend_from_slice(&x[i + 1..]);
    with_entry(good, main, out)
}

/// Deterministic pseudo-random bytes.
pub fn noise(len: usize, mut seed: u64) -> Vec<u8> {
    (0..len)
        .map(|_| {
            seed = seed
                .wrapping_mul(6_364_136_223_846_793_005)
                .wrapping_add(1_442_695_040_888_963_407);
            (seed >> 33) as u8
        })
        .collect()
}

/// Insert `fragment` (e.g. an `<Override .../>`) into `[Content_Types].xml`.
pub fn with_content_types(bytes: &[u8], fragment: &str) -> Vec<u8> {
    let ct = String::from_utf8(
        entries(bytes)
            .into_iter()
            .find(|(n, _)| n == "[Content_Types].xml")
            .unwrap()
            .1,
    )
    .unwrap();
    with_entry(
        bytes,
        "[Content_Types].xml",
        ct.replacen("</Types>", &format!("{fragment}</Types>"), 1)
            .into_bytes(),
    )
}

/// Replace `from` by `to` in the text of entry `name`.
pub fn patch_text(bytes: &[u8], name: &str, from: &str, to: &str) -> Vec<u8> {
    let t = String::from_utf8(
        entries(bytes)
            .into_iter()
            .find(|(n, _)| n == name)
            .unwrap()
            .1,
    )
    .unwrap();
    assert!(t.contains(from), "{name} has no {from:?}");
    with_entry(bytes, name, t.replacen(from, to, 1).into_bytes())
}

/// Rename an entry, rewriting the references the package keeps to it:
/// `_rels/.rels`, its own rels part and its content-type override.
pub fn rename_main(bytes: &[u8], old: &str, new: &str) -> Vec<u8> {
    let mut e = entries(bytes);
    let rels_of = |p: &str| {
        let (d, f) = p.rsplit_once('/').unwrap();
        format!("{d}/_rels/{f}.rels")
    };
    for (n, d) in &mut e {
        if n == old {
            *n = new.to_owned();
        } else if *n == rels_of(old) {
            *n = rels_of(new);
        } else if n == "_rels/.rels" || n == "[Content_Types].xml" {
            let t = String::from_utf8(d.clone()).unwrap();
            *d = t.replace(old, new).into_bytes();
        }
    }
    zip_of(&e)
}
