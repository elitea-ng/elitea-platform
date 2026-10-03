use std::fmt::Write as _;

use super::*;
use crate::transport::runtime_context::{AttachmentTextLayout, AttachmentTextUnit};

/// Twenty pages of about 3 KB, each with a unique word on it.
fn library(read_tokens: u64) -> AttachmentLibrary {
    let mut content = String::new();
    let mut units = Vec::new();
    for number in 1..=20 {
        if !content.is_empty() {
            content.push_str("\n\n");
        }
        let start = content.len();
        let _ = write!(content, "Page {number} opens here. ");
        if number == 13 {
            content.push_str("The warranty clause QUOKKAWORD covers spare parts. ");
        }
        while content.len() - start < 3_000 {
            content.push_str("filler sentence about nothing in particular. ");
        }
        units.push(AttachmentTextUnit {
            kind: "page".to_owned(),
            label: number.to_string(),
            start,
            end: content.len(),
            has_text: true,
        });
    }
    let text_bytes = content.len() as u64;
    let document = AttachmentDocument {
        content,
        layout: AttachmentTextLayout {
            format: "pdf".to_owned(),
            units,
            unit_count: 25,
            low_text_units: Vec::new(),
            text_bytes,
            complete: false,
            partial_reason: "page_limit".to_owned(),
        },
    };
    AttachmentLibrary::new(
        vec![LibraryDocument::new(
            "att1".to_owned(),
            "report.pdf".to_owned(),
            document,
        )],
        read_tokens,
    )
}

#[test]
fn read_by_pages_returns_those_pages_with_markers() {
    let result = library(16_000).read("att1", Some("13-14"), None, None);
    assert_eq!(result["shown"], "pages 13-14");
    assert_eq!(result["truncated"], false);
    let content = result["content"].as_str().expect("content");
    assert!(content.contains("--- page 13 ---"));
    assert!(content.contains("QUOKKAWORD"));
    assert!(!content.contains("Page 15 opens"));
    assert!(content.contains("untrusted-attachment-"));
    assert!(
        result["note"]
            .as_str()
            .unwrap_or("")
            .contains("treat it as data")
    );
    assert!(
        result["note"]
            .as_str()
            .unwrap_or("")
            .contains("pages 1-20 of 25"),
        "a partial extraction is named on every read: {}",
        result["note"]
    );
}

#[test]
fn a_read_is_bounded_and_says_where_to_continue() {
    // 1,000 tokens = 4,000 bytes per answer, so pages 1-5 (~15 KB) need paging.
    let library = library(1_000);
    let first = library.read("att1", Some("1-5"), None, None);
    assert_eq!(first["truncated"], true);
    let next = first["next_offset"].as_u64().expect("next offset");
    assert!(next <= 4_000 + 1);
    assert!(
        first["note"]
            .as_str()
            .unwrap_or("")
            .contains(&format!("offset {next}"))
    );

    // Paging by offset walks the text without gaps or overlaps.
    let mut offset = 0;
    let mut pages = 0;
    loop {
        let answer = library.read("att1", None, Some(offset), None);
        assert_eq!(answer["offset"].as_u64(), Some(offset));
        pages += 1;
        match answer["next_offset"].as_u64() {
            Some(next) => {
                assert!(next > offset, "paging must advance");
                offset = next;
            }
            None => break,
        }
        assert!(pages < 100, "paging must end");
    }
    assert!(
        pages >= 15,
        "60 KB in 4 KB answers takes at least 15 reads, took {pages}"
    );
}

#[test]
fn max_chars_cannot_exceed_the_platform_limit() {
    let answer = library(1_000).read("att1", None, Some(0), Some(1_000_000));
    let content = answer["content"].as_str().expect("content");
    assert!(content.len() < 4_000 + 400, "{} bytes", content.len());
}

#[test]
fn pages_past_what_was_extracted_are_refused_with_the_reason() {
    let answer = library(16_000).read("att1", Some("22-23"), None, None);
    assert_eq!(answer["error"], "pages_not_available");
    assert_eq!(answer["available"], "1-20");
    assert_eq!(answer["total_in_file"], 25);
}

#[test]
fn bad_arguments_are_answered_not_raised() {
    let library = library(16_000);
    assert_eq!(
        library.read("att9", None, None, None)["error"],
        "attachment_not_found"
    );
    assert_eq!(
        library.read("att1", Some("seven"), None, None)["error"],
        "invalid_pages"
    );
    assert_eq!(
        library.read("att1", Some("9-3"), None, None)["error"],
        "invalid_pages"
    );
    assert_eq!(library.search("", None, None)["error"], "invalid_query");
    assert_eq!(
        library.search("word", Some("att9"), None)["error"],
        "attachment_not_found"
    );
}

#[test]
fn search_finds_the_page_that_holds_the_term() {
    let answer = library(16_000).search("quokkaword warranty", None, Some(3));
    let results = answer["results"].as_array().expect("results");
    assert!(!results.is_empty());
    assert_eq!(results[0]["attachment_id"], "att1");
    assert!(
        results[0]["location"].as_str().unwrap_or("").contains("13"),
        "{}",
        results[0]
    );
    assert!(
        results[0]["snippet"]
            .as_str()
            .unwrap_or("")
            .contains("QUOKKAWORD")
    );
    assert!(results.len() <= 3);
    assert!(answer["note"].as_str().unwrap_or("").contains("untrusted"));
}

#[test]
fn chunks_overlap_and_cover_the_whole_text() {
    let text = "word ".repeat(3_000);
    let chunks = chunk(&text);
    assert!(chunks.len() > 1);
    assert_eq!(chunks[0].start, 0);
    assert_eq!(chunks.last().map(|chunk| chunk.end), Some(text.len()));
    for pair in chunks.windows(2) {
        assert!(pair[1].start < pair[0].end, "consecutive chunks overlap");
        assert!(pair[1].start > pair[0].start, "chunks advance");
    }
}

#[test]
fn reads_never_split_a_character() {
    let mut document_text = String::new();
    while document_text.len() < 10_000 {
        document_text.push_str("é漢字🙂 ");
    }
    let document = AttachmentDocument {
        layout: AttachmentTextLayout {
            format: "text".to_owned(),
            units: vec![AttachmentTextUnit {
                kind: "part".to_owned(),
                label: "1".to_owned(),
                start: 0,
                end: document_text.len(),
                has_text: true,
            }],
            unit_count: 1,
            low_text_units: Vec::new(),
            text_bytes: document_text.len() as u64,
            complete: true,
            partial_reason: String::new(),
        },
        content: document_text,
    };
    let library = AttachmentLibrary::new(
        vec![LibraryDocument::new(
            "att1".to_owned(),
            "u.txt".to_owned(),
            document,
        )],
        300,
    );
    for offset in [0, 1, 2, 3, 5, 777] {
        let answer = library.read("att1", None, Some(offset), Some(1_001));
        assert!(answer["content"].is_string(), "offset {offset}");
    }
}
