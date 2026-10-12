//! The service over a REAL Unix socket, with generated fixtures: a PDF, a
//! DOCX, an XLSX and a PPTX small enough to be built in the test (no
//! committed binaries), each with a sentence the text must carry; then
//! every skip reason the service can state, and the protocol refusals.

#![allow(
    clippy::format_collect,
    reason = "the fixtures' XML is assembled from small formatted pieces"
)]

use std::io::Write as _;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use elitea_doc_extractor::client;
use elitea_doc_extractor::pb::document_extract_service_server::DocumentExtractServiceServer;
use elitea_doc_extractor::pb::{
    ExtractHeader, ExtractLimits, ExtractRequest, ExtractResponse, SkipReason, extract_request,
    extract_response,
};
use elitea_doc_extractor::service::{Caps, Exhausted, Extractor};
use tokio::sync::oneshot;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Channel, Server};

const DOCX: &str = "application/vnd.openxmlformats-officedocument.wordprocessingml.document";
const XLSX: &str = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet";
const PPTX: &str = "application/vnd.openxmlformats-officedocument.presentationml.presentation";

static NEXT: AtomicUsize = AtomicUsize::new(0);

/// Counts the exhaustion notices instead of exiting.
#[derive(Default)]
struct Counting(AtomicUsize);

impl Exhausted for Counting {
    fn slots_exhausted(&self) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}

struct Served {
    channel: Channel,
    socket: PathBuf,
    stop: Option<oneshot::Sender<()>>,
    exhausted: Arc<Counting>,
}

impl Drop for Served {
    fn drop(&mut self) {
        if let Some(stop) = self.stop.take() {
            let _ = stop.send(());
        }
        let _ = std::fs::remove_file(&self.socket);
    }
}

fn serve(caps: Caps, concurrency: usize) -> Served {
    let n = NEXT.fetch_add(1, Ordering::SeqCst);
    // A short path: macOS caps a socket path at 104 bytes.
    let socket = std::env::temp_dir().join(format!("dx-{}-{n}.sock", std::process::id()));
    let listener = elitea_engine_sidecar::bind(&socket).expect("bind");
    let exhausted = Arc::new(Counting::default());
    let service = Extractor::new(
        caps,
        concurrency,
        Duration::from_millis(500),
        Arc::clone(&exhausted) as Arc<dyn Exhausted>,
    );
    let (stop, stopped) = oneshot::channel::<()>();
    tokio::spawn(
        Server::builder()
            .add_service(DocumentExtractServiceServer::new(service))
            .serve_with_incoming_shutdown(UnixListenerStream::new(listener), async {
                let _ = stopped.await;
            }),
    );
    let channel = client::channel(&socket).expect("channel");
    Served {
        channel,
        socket,
        stop: Some(stop),
        exhausted,
    }
}

fn header(filename: &str, content_type: &str, length: usize) -> ExtractHeader {
    ExtractHeader {
        filename: filename.to_owned(),
        content_type: content_type.to_owned(),
        content_length: length as u64,
        limits: None,
    }
}

fn text_of(response: ExtractResponse) -> String {
    match response.outcome {
        Some(extract_response::Outcome::Text(text)) => {
            assert_eq!(text.extractor, "xberg");
            text.text
        }
        other => panic!("expected text, got {other:?}"),
    }
}

fn skip_of(response: ExtractResponse) -> (SkipReason, String) {
    match response.outcome {
        Some(extract_response::Outcome::Skip(skip)) => (
            SkipReason::try_from(skip.reason).expect("known reason"),
            skip.detail,
        ),
        other => panic!("expected a skip, got {other:?}"),
    }
}

// ── fixtures ────────────────────────────────────────────────────────────────

/// A PDF with one page per entry, each showing its text in Helvetica. The
/// cross-reference table is exact.
fn pdf(pages: &[&str]) -> Vec<u8> {
    pdf_with(pages, None)
}

/// [`pdf`] with an extra trailer entry and the object it points at (the
/// encrypted fixture).
fn pdf_with(pages: &[&str], extra: Option<(&str, &str)>) -> Vec<u8> {
    let mut objects: Vec<String> = Vec::new();
    let first_page_object = 4;
    let kids: Vec<String> = (0..pages.len())
        .map(|i| format!("{} 0 R", first_page_object + 2 * i))
        .collect();
    objects.push("<< /Type /Catalog /Pages 2 0 R >>".to_owned());
    objects.push(format!(
        "<< /Type /Pages /Kids [{}] /Count {} >>",
        kids.join(" "),
        pages.len()
    ));
    objects.push("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>".to_owned());
    for (i, text) in pages.iter().enumerate() {
        let content_object = first_page_object + 2 * i + 1;
        objects.push(format!(
            "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents {content_object} 0 R >>"
        ));
        let escaped = text
            .replace('\\', "\\\\")
            .replace('(', "\\(")
            .replace(')', "\\)");
        let stream = format!("BT /F1 12 Tf 72 720 Td ({escaped}) Tj ET");
        objects.push(format!(
            "<< /Length {} >>\nstream\n{stream}\nendstream",
            stream.len()
        ));
    }
    let mut trailer_extra = String::new();
    if let Some((entry, object)) = extra {
        objects.push(object.to_owned());
        trailer_extra = entry.replace("{N}", &objects.len().to_string());
    }
    let mut out = Vec::new();
    out.extend_from_slice(b"%PDF-1.4\n");
    let mut offsets = Vec::new();
    for (i, object) in objects.iter().enumerate() {
        offsets.push(out.len());
        out.extend_from_slice(format!("{} 0 obj\n{object}\nendobj\n", i + 1).as_bytes());
    }
    let xref = out.len();
    out.extend_from_slice(format!("xref\n0 {}\n", objects.len() + 1).as_bytes());
    out.extend_from_slice(b"0000000000 65535 f \n");
    for offset in offsets {
        out.extend_from_slice(format!("{offset:010} 00000 n \n").as_bytes());
    }
    out.extend_from_slice(
        format!(
            "trailer\n<< /Size {} /Root 1 0 R {trailer_extra} >>\nstartxref\n{xref}\n%%EOF\n",
            objects.len() + 1
        )
        .as_bytes(),
    );
    out
}

/// A PDF whose trailer names a standard security handler with owner and
/// user passwords that are not the empty one: a reader without the
/// password cannot open it.
fn encrypted_pdf() -> Vec<u8> {
    let keys = "A".repeat(64);
    pdf_with(
        &["Confidential refund policy"],
        Some((
            "/Encrypt {N} 0 R /ID [<0123456789abcdef0123456789abcdef> <0123456789abcdef0123456789abcdef>]",
            &format!(
                "<< /Filter /Standard /V 1 /R 2 /Length 40 /P -3904 /O <{keys}> /U <{keys}> >>"
            ),
        )),
    )
}

/// A zip with the given entries, stored (no compression feature needed).
fn zip(entries: &[(&str, String)]) -> Vec<u8> {
    let mut writer = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
    let options =
        zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Stored);
    for (name, body) in entries {
        writer.start_file(*name, options).expect("entry");
        writer.write_all(body.as_bytes()).expect("body");
    }
    writer.finish().expect("finish").into_inner()
}

const RELS: &str = r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="{TARGET}"/>
</Relationships>"#;

fn docx(paragraphs: &[&str]) -> Vec<u8> {
    let body: String = paragraphs
        .iter()
        .map(|p| format!("<w:p><w:r><w:t>{p}</w:t></w:r></w:p>"))
        .collect();
    zip(&[
        (
            "[Content_Types].xml",
            r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>"#
                .to_owned(),
        ),
        ("_rels/.rels", RELS.replace("{TARGET}", "word/document.xml")),
        (
            "word/document.xml",
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>{body}</w:body></w:document>"#
            ),
        ),
    ])
}

fn xlsx(cells: &[&str]) -> Vec<u8> {
    let rows: String = cells
        .iter()
        .enumerate()
        .map(|(i, text)| {
            format!(
                r#"<row r="{r}"><c r="A{r}" t="inlineStr"><is><t>{text}</t></is></c></row>"#,
                r = i + 1
            )
        })
        .collect();
    zip(&[
        (
            "[Content_Types].xml",
            r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>"#
                .to_owned(),
        ),
        ("_rels/.rels", RELS.replace("{TARGET}", "xl/workbook.xml")),
        (
            "xl/workbook.xml",
            r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Policy" sheetId="1" r:id="rId1"/></sheets></workbook>"#
                .to_owned(),
        ),
        (
            "xl/_rels/workbook.xml.rels",
            r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>"#
                .to_owned(),
        ),
        (
            "xl/worksheets/sheet1.xml",
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>{rows}</sheetData></worksheet>"#
            ),
        ),
    ])
}

fn pptx(slides: &[&str]) -> Vec<u8> {
    let mut entries: Vec<(String, String)> = vec![
        (
            "[Content_Types].xml".to_owned(),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
{}
</Types>"#,
                (1..=slides.len())
                    .map(|i| format!(
                        r#"<Override PartName="/ppt/slides/slide{i}.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>"#
                    ))
                    .collect::<String>()
            ),
        ),
        (
            "_rels/.rels".to_owned(),
            RELS.replace("{TARGET}", "ppt/presentation.xml"),
        ),
        (
            "ppt/presentation.xml".to_owned(),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst>{}</p:sldIdLst></p:presentation>"#,
                (1..=slides.len())
                    .map(|i| format!(r#"<p:sldId id="{}" r:id="rId{i}"/>"#, 255 + i))
                    .collect::<String>()
            ),
        ),
        (
            "ppt/_rels/presentation.xml.rels".to_owned(),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">{}</Relationships>"#,
                (1..=slides.len())
                    .map(|i| format!(
                        r#"<Relationship Id="rId{i}" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide{i}.xml"/>"#
                    ))
                    .collect::<String>()
            ),
        ),
    ];
    for (i, text) in slides.iter().enumerate() {
        entries.push((
            format!("ppt/slides/slide{}.xml", i + 1),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>{text}</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>"#
            ),
        ));
    }
    let borrowed: Vec<(&str, String)> = entries
        .iter()
        .map(|(name, body)| (name.as_str(), body.clone()))
        .collect();
    zip(&borrowed)
}

// ── the formats ─────────────────────────────────────────────────────────────

#[tokio::test(flavor = "multi_thread")]
async fn pdf_docx_xlsx_and_pptx_give_their_text() {
    let served = serve(Caps::default(), 2);
    let cases: [(&str, &str, Vec<u8>, &str); 4] = [
        (
            "policy.pdf",
            "application/pdf",
            pdf(&[
                "Refunds within thirty days.",
                "Exchanges within sixty days.",
            ]),
            "Refunds within thirty days",
        ),
        (
            "policy.docx",
            DOCX,
            docx(&["Refunds go through Billing.", "Second paragraph."]),
            "Refunds go through Billing",
        ),
        (
            "policy.xlsx",
            XLSX,
            xlsx(&["Refund window", "thirty days"]),
            "Refund window",
        ),
        (
            "policy.pptx",
            PPTX,
            pptx(&["Refund policy overview", "Questions"]),
            "Refund policy overview",
        ),
    ];
    for (name, mime, bytes, phrase) in cases {
        let response = client::extract(
            served.channel.clone(),
            header(name, mime, bytes.len()),
            bytes,
        )
        .await
        .unwrap_or_else(|status| panic!("{name}: {status}"));
        let text = text_of(response);
        assert!(text.contains(phrase), "{name}: {text:?}");
    }
    // The second PDF page made it too.
    let two = pdf(&["First page words.", "Second page words."]);
    let text = text_of(
        client::extract(
            served.channel.clone(),
            header("two.pdf", "application/pdf", two.len()),
            two,
        )
        .await
        .expect("two pages"),
    );
    assert!(text.contains("Second page words"), "{text:?}");
}

#[tokio::test(flavor = "multi_thread")]
async fn capabilities_names_the_formats_and_the_caps() {
    let caps = Caps {
        max_input_bytes: 1234,
        max_pages: 5,
        max_output_bytes: 999,
        timeout: Duration::from_millis(777),
    };
    let served = serve(caps, 1);
    let answer = client::capabilities(served.channel.clone())
        .await
        .expect("capabilities");
    assert!(
        answer.content_types.iter().any(|t| t == "application/pdf")
            && answer.content_types.iter().any(|t| t == DOCX),
        "{:?}",
        answer.content_types
    );
    assert_eq!(
        answer.limits,
        Some(ExtractLimits {
            max_input_bytes: 1234,
            max_pages: 5,
            max_output_bytes: 999,
            timeout_millis: 777,
        })
    );
    assert_eq!(answer.version, env!("CARGO_PKG_VERSION"));
}

// ── the skip reasons ────────────────────────────────────────────────────────

#[tokio::test(flavor = "multi_thread")]
async fn a_corrupt_document_is_skipped_corrupt() {
    let served = serve(Caps::default(), 1);
    let bytes = b"%PDF-1.4 but nothing follows".to_vec();
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("broken.pdf", "application/pdf", bytes.len()),
            bytes,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::Corrupt, "{detail}");
    // An Office container that is not a zip.
    let bytes = b"PK\x03\x04 truncated".to_vec();
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("broken.docx", DOCX, bytes.len()),
            bytes,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::Corrupt, "{detail}");
}

#[tokio::test(flavor = "multi_thread")]
async fn an_encrypted_pdf_is_skipped_encrypted() {
    let served = serve(Caps::default(), 1);
    let bytes = encrypted_pdf();
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("secret.pdf", "application/pdf", bytes.len()),
            bytes,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::Encrypted, "{detail}");
}

#[tokio::test(flavor = "multi_thread")]
async fn too_many_pages_is_refused_not_truncated() {
    let served = serve(
        Caps {
            max_pages: 2,
            ..Caps::default()
        },
        1,
    );
    let bytes = pdf(&["One.", "Two.", "Three."]);
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("long.pdf", "application/pdf", bytes.len()),
            bytes,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::TooManyPages, "{detail}");
    // The request's own, lower page limit applies the same way.
    let served = serve(Caps::default(), 1);
    let bytes = pdf(&["One.", "Two.", "Three."]);
    let mut asked = header("long.pdf", "application/pdf", bytes.len());
    asked.limits = Some(ExtractLimits {
        max_pages: 2,
        ..ExtractLimits::default()
    });
    let (reason, _) = skip_of(
        client::extract(served.channel.clone(), asked, bytes)
            .await
            .expect("answered"),
    );
    assert_eq!(reason, SkipReason::TooManyPages);
}

#[tokio::test(flavor = "multi_thread")]
async fn too_large_is_refused_on_the_declared_length_and_on_the_bytes() {
    let served = serve(
        Caps {
            max_input_bytes: 600,
            ..Caps::default()
        },
        1,
    );
    let bytes = pdf(&["Small."]);
    // Declared over the cap: refused before a byte is read.
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("big.pdf", "application/pdf", 10_000),
            bytes.clone(),
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::TooLarge, "{detail}");
    assert!(detail.contains("declares 10000 bytes"), "{detail}");
    // Declared small, but more bytes arrive than the cap.
    let big = pdf(&["A page.", "Another page.", "A third page with more words."]);
    assert!(big.len() > 600, "{}", big.len());
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("big.pdf", "application/pdf", 0),
            big,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::TooLarge, "{detail}");
    assert!(detail.contains("over the 600-byte limit"), "{detail}");
    // A request cannot raise the cap.
    let mut asked = header("big.pdf", "application/pdf", 10_000);
    asked.limits = Some(ExtractLimits {
        max_input_bytes: 1 << 30,
        ..ExtractLimits::default()
    });
    let (reason, _) = skip_of(
        client::extract(served.channel.clone(), asked, bytes)
            .await
            .expect("answered"),
    );
    assert_eq!(reason, SkipReason::TooLarge);
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unsupported_type_is_skipped_without_reading_the_bytes() {
    let served = serve(Caps::default(), 1);
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("logo.png", "image/png", 4),
            b"\x89PNG".to_vec(),
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::Unsupported);
    assert!(detail.contains("image/png"), "{detail}");
    // Text is the worker's own business, not the extractor's.
    let (reason, _) = skip_of(
        client::extract(
            served.channel.clone(),
            header("README.md", "text/markdown", 2),
            b"# hi".to_vec(),
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::Unsupported);
}

#[tokio::test(flavor = "multi_thread")]
async fn output_over_the_cap_is_refused() {
    let served = serve(
        Caps {
            max_output_bytes: 8,
            ..Caps::default()
        },
        1,
    );
    let bytes = pdf(&["Refunds within thirty days."]);
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("policy.pdf", "application/pdf", bytes.len()),
            bytes,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::OutputTooLarge, "{detail}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_document_without_text_is_skipped_empty() {
    let served = serve(Caps::default(), 1);
    let bytes = docx(&[]);
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("empty.docx", DOCX, bytes.len()),
            bytes,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::Empty, "{detail}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_timeout_is_a_skip_and_exhausting_every_slot_is_reported_once() {
    // One slot, a timeout no extraction can meet: the first request times
    // out and holds the slot, which is every slot there is.
    let served = serve(
        Caps {
            timeout: Duration::from_millis(1),
            ..Caps::default()
        },
        1,
    );
    let pages: Vec<String> = (0..400)
        .map(|i| format!("Page {i} of a long document."))
        .collect();
    let refs: Vec<&str> = pages.iter().map(String::as_str).collect();
    let bytes = pdf(&refs);
    let (reason, detail) = skip_of(
        client::extract(
            served.channel.clone(),
            header("slow.pdf", "application/pdf", bytes.len()),
            bytes,
        )
        .await
        .expect("answered"),
    );
    assert_eq!(reason, SkipReason::Timeout, "{detail}");
    assert_eq!(served.exhausted.0.load(Ordering::SeqCst), 1);
}

// ── the protocol ────────────────────────────────────────────────────────────

#[tokio::test(flavor = "multi_thread")]
async fn a_stream_without_a_header_is_invalid() {
    let served = serve(Caps::default(), 1);
    let mut client =
        elitea_doc_extractor::pb::document_extract_service_client::DocumentExtractServiceClient::new(
            served.channel.clone(),
        );
    let status = client
        .extract(tokio_stream::iter(vec![ExtractRequest {
            message: Some(extract_request::Message::Chunk(b"%PDF".to_vec())),
        }]))
        .await
        .expect_err("refused");
    assert_eq!(status.code(), tonic::Code::InvalidArgument, "{status}");
    let status = client
        .extract(tokio_stream::iter(Vec::<ExtractRequest>::new()))
        .await
        .expect_err("refused");
    assert_eq!(status.code(), tonic::Code::InvalidArgument, "{status}");
    let status = client
        .extract(tokio_stream::iter(vec![ExtractRequest {
            message: Some(extract_request::Message::Header(header("x.pdf", "", 0))),
        }]))
        .await
        .expect_err("refused");
    assert_eq!(status.code(), tonic::Code::InvalidArgument, "{status}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_second_header_is_invalid() {
    let served = serve(Caps::default(), 1);
    let mut client =
        elitea_doc_extractor::pb::document_extract_service_client::DocumentExtractServiceClient::new(
            served.channel.clone(),
        );
    let status = client
        .extract(tokio_stream::iter(vec![
            ExtractRequest {
                message: Some(extract_request::Message::Header(header(
                    "a.pdf",
                    "application/pdf",
                    0,
                ))),
            },
            ExtractRequest {
                message: Some(extract_request::Message::Header(header(
                    "b.pdf",
                    "application/pdf",
                    0,
                ))),
            },
        ]))
        .await
        .expect_err("refused");
    assert_eq!(status.code(), tonic::Code::InvalidArgument, "{status}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_stream_that_goes_quiet_is_a_deadline() {
    let served = serve(Caps::default(), 1);
    let mut client =
        elitea_doc_extractor::pb::document_extract_service_client::DocumentExtractServiceClient::new(
            served.channel.clone(),
        );
    let (sender, receiver) = tokio::sync::mpsc::channel::<ExtractRequest>(4);
    sender
        .send(ExtractRequest {
            message: Some(extract_request::Message::Header(header(
                "quiet.pdf",
                "application/pdf",
                0,
            ))),
        })
        .await
        .expect("sent");
    // Never send a chunk, never close: the server's idle limit (500 ms in
    // this harness) must end the call.
    let status = client
        .extract(tokio_stream::wrappers::ReceiverStream::new(receiver))
        .await
        .expect_err("refused");
    assert_eq!(status.code(), tonic::Code::DeadlineExceeded, "{status}");
    drop(sender);
}
