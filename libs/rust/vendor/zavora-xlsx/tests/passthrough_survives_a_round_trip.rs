//! Elitea patch (zip 9): a part the workbook model does not know survives
//! open -> save -> reopen, a part whose name is not UTF-8 included.
//!
//! `Workbook::open_from_buffer` carries every unknown part as a pass-through
//! entry and writes it back on save. zip 9 decodes an entry name on demand,
//! and the decode is fallible, so the pass-through scan must neither drop a
//! part silently nor lose its bytes. zip 9.1 reads a name that is not UTF-8
//! as CP437, so the part is kept under that reading.

use std::io::{Cursor, Read, Write};

use zavora_xlsx::Workbook;
use zip::write::SimpleFileOptions;

const CUSTOM: &str = "customXml/item1.xml";
const CUSTOM_BODY: &[u8] = b"<root>kept</root>";
/// Name bytes that are not UTF-8.
const RAW_NAME: &[u8] = b"customXml/\xff\xfe-opaque.bin";
const RAW_BODY: &[u8] = b"opaque bytes";

/// A saved workbook, plus a custom part and a part named `RAW_NAME`. zip 9
/// writes a name only from a `str`, so that part is written under an ASCII
/// placeholder of the same length, whose bytes are then replaced in the
/// local and the central header.
fn workbook_with_extra_parts() -> Vec<u8> {
    let mut book = Workbook::new();
    book.worksheet(0).unwrap().write(0, 0, "cell").unwrap();
    let saved = book.save_to_buffer().unwrap();

    let mut src = zip::ZipArchive::new(Cursor::new(saved)).unwrap();
    let mut w = zip::ZipWriter::new(Cursor::new(Vec::new()));
    for i in 0..src.len() {
        w.raw_copy_file(src.by_index_raw(i).unwrap()).unwrap();
    }
    let opts = SimpleFileOptions::default();
    w.start_file(CUSTOM, opts).unwrap();
    w.write_all(CUSTOM_BODY).unwrap();
    let placeholder = "x".repeat(RAW_NAME.len());
    w.start_file(placeholder.as_str(), opts).unwrap();
    w.write_all(RAW_BODY).unwrap();
    let mut bytes = w.finish().unwrap().into_inner();

    let from = placeholder.as_bytes();
    let mut replaced = 0;
    let mut i = 0;
    while i + from.len() <= bytes.len() {
        if &bytes[i..i + from.len()] == from {
            bytes[i..i + from.len()].copy_from_slice(RAW_NAME);
            replaced += 1;
            i += from.len();
        } else {
            i += 1;
        }
    }
    assert_eq!(replaced, 2, "the local and the central header name");
    bytes
}

/// Every entry of `bytes` whose content is `body`, by decoded name.
fn names_holding(bytes: &[u8], body: &[u8]) -> Vec<String> {
    let mut zip = zip::ZipArchive::new(Cursor::new(bytes)).unwrap();
    let mut out = Vec::new();
    for i in 0..zip.len() {
        let mut e = zip.by_index(i).unwrap();
        let mut got = Vec::new();
        e.read_to_end(&mut got).unwrap();
        if got == body {
            out.push(e.name().unwrap().into_owned());
        }
    }
    out
}

#[test]
fn unknown_parts_survive_open_save_reopen() {
    let original = workbook_with_extra_parts();
    {
        let zip = zip::ZipArchive::new(Cursor::new(&original)).unwrap();
        assert!(
            zip.file_names_raw().any(|n| n == RAW_NAME),
            "the fixture must hold the name bytes that are not UTF-8"
        );
    }

    let mut book = Workbook::open_from_buffer(&original).expect("the workbook opens");
    let carried: Vec<&str> = book
        .passthrough_entries()
        .iter()
        .map(|(n, _)| n.as_str())
        .collect();
    assert!(carried.contains(&CUSTOM), "{carried:?}");
    assert_eq!(
        book.passthrough_entries()
            .iter()
            .filter(|(_, b)| b == RAW_BODY)
            .count(),
        1,
        "the part with the non-UTF-8 name is carried, not dropped: {carried:?}"
    );

    book.worksheet(0).unwrap().write(1, 0, "edited").unwrap();
    let saved = book.save_to_buffer().expect("the workbook saves");

    assert_eq!(names_holding(&saved, CUSTOM_BODY), vec![CUSTOM.to_owned()]);
    // The part keeps its content and the name zip decodes from its bytes.
    let decoded = {
        let zip = zip::ZipArchive::new(Cursor::new(&original)).unwrap();
        let i = zip.file_names_raw().position(|n| n == RAW_NAME).unwrap();
        zip.name_for_index(i).unwrap().unwrap().into_owned()
    };
    assert_eq!(names_holding(&saved, RAW_BODY), vec![decoded]);

    // And the saved file opens again, with the edit and both parts.
    let again = Workbook::open_from_buffer(&saved).expect("the saved workbook reopens");
    assert_eq!(
        again
            .passthrough_entries()
            .iter()
            .filter(|(_, b)| b == CUSTOM_BODY || b == RAW_BODY)
            .count(),
        2
    );
}
