//! The sidecar through its real binary: the bytes of a fixture on stdin, the
//! JSON answer on stdout, as the desktop host drives it.

use std::io::Write;
use std::path::PathBuf;
use std::process::{Command, Stdio};

use elitea_doc_extract::PageRange;
use elitea_doc_extract::wire::encode_request_head;
use serde_json::Value;

fn fixture(name: &str) -> Vec<u8> {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name);
    std::fs::read(&path).unwrap_or_else(|error| panic!("{}: {error}", path.display()))
}

/// Runs the sidecar binary on one request and returns the answer.
fn run(mime: &str, range: &PageRange, bytes: &[u8]) -> Value {
    let mut request = encode_request_head(mime, range).expect("head");
    request.extend_from_slice(bytes);
    run_raw(&request)
}

fn run_raw(request: &[u8]) -> Value {
    let mut child = Command::new(env!("CARGO_BIN_EXE_elitea-doc-extract-sidecar"))
        .env_clear()
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn");
    let mut stdin = child.stdin.take().expect("stdin");
    let request = request.to_vec();
    let writer = std::thread::spawn(move || {
        let _ = stdin.write_all(&request);
    });
    let output = child.wait_with_output().expect("output");
    writer.join().expect("writer");
    serde_json::from_slice(&output.stdout)
        .unwrap_or_else(|error| panic!("{error}: {}", String::from_utf8_lossy(&output.stdout)))
}

fn texts(answer: &Value) -> Vec<(u64, String)> {
    answer["pages"]
        .as_array()
        .expect("pages")
        .iter()
        .map(|page| {
            (
                page["n"].as_u64().expect("n"),
                page["text"].as_str().expect("text").to_owned(),
            )
        })
        .collect()
}

const PDF: &str = "application/pdf";
const DOCX: &str = "application/vnd.openxmlformats-officedocument.wordprocessingml.document";
const XLSX: &str = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet";
const PPTX: &str = "application/vnd.openxmlformats-officedocument.presentationml.presentation";

#[test]
fn a_pdf_gives_its_text_page_by_page() {
    let answer = run(PDF, &PageRange::ALL, &fixture("policy.pdf"));
    assert_eq!(answer["ok"], true, "{answer}");
    assert_eq!(answer["page_count"], 3);
    assert_eq!(answer["extractor"], "xberg");
    let pages = texts(&answer);
    assert_eq!(pages.len(), 3, "{pages:?}");
    assert!(pages[0].1.contains("Refund policy page one"), "{pages:?}");
    assert!(pages[1].1.contains("Escalation page two"), "{pages:?}");
    assert!(pages[2].1.contains("Retention page three"), "{pages:?}");
}

#[test]
fn a_page_range_returns_only_those_pages_and_the_whole_count() {
    let range = PageRange::parse("2").expect("range");
    let answer = run(PDF, &range, &fixture("policy.pdf"));
    assert_eq!(answer["ok"], true, "{answer}");
    assert_eq!(answer["page_count"], 3);
    let pages = texts(&answer);
    assert_eq!(pages.len(), 1, "{pages:?}");
    assert_eq!(pages[0].0, 2);
    assert!(pages[0].1.contains("Escalation"), "{pages:?}");

    let open = run(
        PDF,
        &PageRange::parse("2-").expect("r"),
        &fixture("policy.pdf"),
    );
    assert_eq!(texts(&open).iter().map(|p| p.0).collect::<Vec<_>>(), [2, 3]);
    let past = run(
        PDF,
        &PageRange::parse("9").expect("r"),
        &fixture("policy.pdf"),
    );
    assert_eq!(past["ok"], true);
    assert_eq!(texts(&past).len(), 0);
}

#[test]
fn office_files_and_mail_give_their_text() {
    let word = run(DOCX, &PageRange::ALL, &fixture("memo.docx"));
    assert_eq!(word["ok"], true, "{word}");
    let word_text: String = texts(&word).into_iter().map(|p| p.1).collect();
    assert!(word_text.contains("Contract clause alpha"), "{word_text}");
    assert!(word_text.contains("Contract clause beta"), "{word_text}");

    let sheet = run(XLSX, &PageRange::ALL, &fixture("budget.xlsx"));
    assert_eq!(sheet["ok"], true, "{sheet}");
    let sheet_text: String = texts(&sheet).into_iter().map(|p| p.1).collect();
    assert!(sheet_text.contains("Budget line two"), "{sheet_text}");

    let deck = run(PPTX, &PageRange::ALL, &fixture("deck.pptx"));
    assert_eq!(deck["ok"], true, "{deck}");
    let deck_text: String = texts(&deck).into_iter().map(|p| p.1).collect();
    assert!(deck_text.contains("Risks slide two"), "{deck_text}");

    let mail = run("message/rfc822", &PageRange::ALL, &fixture("mail.eml"));
    assert_eq!(mail["ok"], true, "{mail}");
    let mail_text: String = texts(&mail).into_iter().map(|p| p.1).collect();
    assert!(mail_text.contains("quarterly figures"), "{mail_text}");
}

#[test]
fn a_slide_deck_is_paged_by_slide() {
    let deck = run(
        PPTX,
        &PageRange::parse("3").expect("r"),
        &fixture("deck.pptx"),
    );
    assert_eq!(deck["ok"], true, "{deck}");
    assert_eq!(deck["page_count"], 3, "{deck}");
    let pages = texts(&deck);
    assert_eq!(pages.len(), 1, "{pages:?}");
    assert!(pages[0].1.contains("Asks slide three"), "{pages:?}");
}

#[test]
fn hostile_and_malformed_requests_are_answered_not_crashed() {
    let garbage = run(PDF, &PageRange::ALL, b"%PDF-1.7 this is not a pdf at all");
    assert_eq!(garbage["ok"], false, "{garbage}");
    assert_eq!(garbage["error"], "unreadable");

    let truncated = fixture("policy.pdf");
    let cut = run(PDF, &PageRange::ALL, &truncated[..truncated.len() / 2]);
    assert_eq!(cut["ok"], false, "{cut}");

    let zip_bomb_shape = run(DOCX, &PageRange::ALL, b"PK\x03\x04 not really a zip");
    assert_eq!(zip_bomb_shape["ok"], false, "{zip_bomb_shape}");

    let unknown = run("application/x-unknown", &PageRange::ALL, b"abc");
    assert_eq!(unknown["error"], "unsupported", "{unknown}");

    for request in [&b""[..], b"EDX1", b"NOPE0000000000000000", &[b'E'; 40]] {
        let answer = run_raw(request);
        assert_eq!(answer["ok"], false, "{answer}");
        assert_eq!(answer["error"], "unreadable");
    }
}

#[test]
fn open_formats_and_books_give_their_text() {
    for (file, mime, needle) in [
        (
            "notes.odt",
            "application/vnd.oasis.opendocument.text",
            "Open document paragraph two",
        ),
        (
            "letter.rtf",
            "application/rtf",
            "Rental agreement clause nine",
        ),
        ("book.epub", "application/epub+zip", "Heron sightings"),
    ] {
        let answer = run(mime, &PageRange::ALL, &fixture(file));
        assert_eq!(answer["ok"], true, "{file}: {answer}");
        let text: String = texts(&answer).into_iter().map(|p| p.1).collect();
        assert!(text.contains(needle), "{file}: {text}");
    }
}
