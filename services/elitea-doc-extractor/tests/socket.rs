//! The service over a REAL Unix socket, with generated fixtures: a PDF, a
//! DOCX, an XLSX and a PPTX small enough to be built in the test (no
//! committed binaries), each with a sentence the text must carry; then
//! every skip reason the service can state, and the protocol refusals.

#![allow(
    clippy::format_collect,
    reason = "the fixtures' XML is assembled from small formatted pieces"
)]

use std::collections::BTreeSet;
use std::io::Write as _;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use elitea_doc_extractor::pb::document_extract_service_server::DocumentExtractServiceServer;
use elitea_doc_extractor::pb::{
    ExtractHeader, ExtractImages, ExtractLimits, ExtractRequest, ExtractResponse, ExtractedText,
    SkipReason, extract_request, extract_response,
};
use elitea_doc_extractor::service::{
    Capacity, Caps, Exhausted, Extractor, ImageCaps, outer_timeout,
};
use tokio::sync::oneshot;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Channel, Server};

/// The tests' client over the socket: a lazily connected channel and the two
/// RPCs, the document streamed in chunks the way the worker's client does
/// it (`services/elitea-worker-rust/src/transport/extractor_client.rs`,
/// which is the one production client). The binary's `healthcheck` carries
/// its own bare `Capabilities` call; nothing in the library duplicates this.
mod client {
    use std::path::{Path, PathBuf};
    use std::time::Duration;

    use elitea_doc_extractor::pb::document_extract_service_client::DocumentExtractServiceClient;
    use elitea_doc_extractor::pb::{
        CapabilitiesRequest, CapabilitiesResponse, ExtractHeader, ExtractRequest, ExtractResponse,
        extract_request,
    };
    use hyper_util::rt::TokioIo;
    use prost::bytes::Bytes;
    use tokio::net::UnixStream;
    use tonic::transport::{Channel, Endpoint, Uri};
    use tower::service_fn;

    /// Bytes per streamed chunk (the worker's figure).
    pub(super) const CHUNK_BYTES: usize = 256 * 1024;

    /// A lazily connected channel to the socket at `path`.
    pub(super) fn channel(path: &Path) -> Result<Channel, tonic::transport::Error> {
        let path: PathBuf = path.to_path_buf();
        Ok(Endpoint::try_from("http://extractor.local")?
            .connect_timeout(Duration::from_secs(5))
            .connect_with_connector_lazy(service_fn(move |_: Uri| {
                let path = path.clone();
                async move { UnixStream::connect(path).await.map(TokioIo::new) }
            })))
    }

    pub(super) async fn capabilities(
        channel: Channel,
    ) -> Result<CapabilitiesResponse, tonic::Status> {
        DocumentExtractServiceClient::new(channel)
            .capabilities(CapabilitiesRequest {})
            .await
            .map(tonic::Response::into_inner)
    }

    /// `header`, then `bytes` in [`CHUNK_BYTES`] slices of one buffer.
    pub(super) async fn extract(
        channel: Channel,
        header: ExtractHeader,
        bytes: Vec<u8>,
    ) -> Result<ExtractResponse, tonic::Status> {
        let bytes = Bytes::from(bytes);
        let chunks = (0..bytes.len()).step_by(CHUNK_BYTES).map(move |start| {
            let end = (start + CHUNK_BYTES).min(bytes.len());
            ExtractRequest {
                message: Some(extract_request::Message::Chunk(bytes.slice(start..end))),
            }
        });
        let header = std::iter::once(ExtractRequest {
            message: Some(extract_request::Message::Header(header)),
        });
        DocumentExtractServiceClient::new(channel)
            .extract(tokio_stream::iter(header.chain(chunks)))
            .await
            .map(tonic::Response::into_inner)
    }
}

const DOCX: &str = "application/vnd.openxmlformats-officedocument.wordprocessingml.document";
const XLSX: &str = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet";
const PPTX: &str = "application/vnd.openxmlformats-officedocument.presentationml.presentation";

mod support;
use support::{Block, big_png, docx_with, pdf_with_pictures, png, pptx_with};

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
    service: Arc<Extractor>,
}

/// This process's uid: what the service is told to serve, as the worker's
/// uid is in a deployment.
fn own_uid() -> u32 {
    rustix::process::getuid().as_raw()
}

/// The harness capacity: `concurrency` slots, a 500 ms idle limit, and a
/// slot wait long enough never to fire in these tests unless a test lowers
/// it.
fn capacity(concurrency: usize) -> Capacity {
    Capacity {
        concurrency,
        slot_wait: Duration::from_secs(30),
        stream_idle: Duration::from_millis(500),
    }
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
    serve_with(caps, capacity(concurrency), BTreeSet::from([own_uid()]))
}

fn serve_with(caps: Caps, capacity: Capacity, peer_uids: BTreeSet<u32>) -> Served {
    serve_delayed(caps, capacity, peer_uids, Duration::ZERO)
}

/// [`serve_with`] whose every extraction thread pauses `delay` before it
/// extracts: a document that takes that long.
fn serve_delayed(
    caps: Caps,
    capacity: Capacity,
    peer_uids: BTreeSet<u32>,
    delay: Duration,
) -> Served {
    serve_full(caps, capacity, peer_uids, delay, ImageCaps::default())
}

/// [`serve_delayed`] with the caps on returned pictures.
fn serve_full(
    caps: Caps,
    capacity: Capacity,
    peer_uids: BTreeSet<u32>,
    delay: Duration,
    image_caps: ImageCaps,
) -> Served {
    let n = NEXT.fetch_add(1, Ordering::SeqCst);
    // A short path: macOS caps a socket path at 104 bytes.
    let socket = std::env::temp_dir().join(format!("dx-{}-{n}.sock", std::process::id()));
    let listener = elitea_engine_sidecar::bind(&socket).expect("bind");
    let exhausted = Arc::new(Counting::default());
    let service = Arc::new(
        Extractor::new(
            caps,
            capacity,
            peer_uids,
            Arc::clone(&exhausted) as Arc<dyn Exhausted>,
        )
        .with_extraction_delay(delay)
        .with_image_caps(image_caps),
    );
    let (stop, stopped) = oneshot::channel::<()>();
    tokio::spawn(
        Server::builder()
            .add_service(DocumentExtractServiceServer::from_arc(Arc::clone(&service)))
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
        service,
    }
}

fn header(filename: &str, content_type: &str, length: usize) -> ExtractHeader {
    ExtractHeader {
        filename: filename.to_owned(),
        content_type: content_type.to_owned(),
        content_length: length as u64,
        limits: None,
        images: None,
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
    // The capacity a client sizes its queue and deadline from.
    assert_eq!(answer.concurrency, 1);
    assert_eq!(answer.slot_wait_millis, 30_000);
    assert_eq!(answer.stream_idle_millis, 500);
    // The load, idle: nothing held, nothing overrun; healthy.
    assert_eq!((answer.busy, answer.overruns), (0, 0));
    assert!(elitea_doc_extractor::service::is_healthy(&answer));
    // With the one slot held (as an extraction would) the answer says so.
    let held = served.service.hold_slot().await;
    let busy = client::capabilities(served.channel.clone())
        .await
        .expect("capabilities");
    assert_eq!((busy.busy, busy.overruns), (1, 0));
    assert!(
        elitea_doc_extractor::service::is_healthy(&busy),
        "busy is not wedged"
    );
    drop(held);
}

// ── the peer check ──────────────────────────────────────────────────────────

#[tokio::test(flavor = "multi_thread")]
async fn a_peer_outside_the_configured_uids_is_refused_extract_but_served_the_health_probe() {
    // Served uids: anything but this process. Extract is refused before a
    // byte of the document is read; nothing is extracted, nothing is
    // counted. Capabilities from this process's OWN uid is served all the
    // same: it is the health probe (`elitea-doc-extractor healthcheck`, the
    // binary asking itself), and a deployment whose worker runs as another
    // uid must not fail its liveness probe for it.
    let served = serve_with(
        Caps::default(),
        capacity(1),
        BTreeSet::from([own_uid().wrapping_add(1)]),
    );
    let answer = client::capabilities(served.channel.clone())
        .await
        .expect("the probe is served to the process's own uid");
    assert_eq!(answer.concurrency, 1);
    assert_eq!((answer.busy, answer.overruns), (0, 0));
    let bytes = pdf(&["Never read."]);
    let status = client::extract(
        served.channel.clone(),
        header("peer.pdf", "application/pdf", bytes.len()),
        bytes,
    )
    .await
    .expect_err("refused");
    assert_eq!(status.code(), tonic::Code::PermissionDenied, "{status}");
    assert!(status.message().contains("uid"), "{status}");
    assert_eq!(served.service.overruns(), 0);
    assert_eq!(served.service.held_body_bytes(), 0);
    // The same socket, this uid among the served: answered.
    let allowed = serve_with(
        Caps::default(),
        capacity(1),
        BTreeSet::from([own_uid().wrapping_add(1), own_uid()]),
    );
    client::capabilities(allowed.channel.clone())
        .await
        .expect("served");
}

// ── the slot wait is apart from the extraction timeout ──────────────────────

#[tokio::test(flavor = "multi_thread")]
async fn a_queued_request_is_served_when_a_slot_frees_not_refused_at_the_timeout() {
    // One slot, held (as an overrun would) for longer than the EXTRACTION
    // timeout (50 ms) but shorter than the SLOT wait (2 s): the queued
    // request is served with its text once the slot frees. Under the old
    // coupling it would have been RESOURCE_EXHAUSTED at 50 ms.
    let served = serve_with(
        Caps {
            timeout: Duration::from_millis(50),
            ..Caps::default()
        },
        Capacity {
            concurrency: 1,
            slot_wait: Duration::from_secs(2),
            stream_idle: Duration::from_millis(500),
        },
        BTreeSet::from([own_uid()]),
    );
    let held = served.service.hold_slot().await;
    let release = tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(300)).await;
        drop(held);
    });
    let bytes = pdf(&["Queued, not refused."]);
    let started = std::time::Instant::now();
    let text = text_of(
        client::extract(
            served.channel.clone(),
            header("queued.pdf", "application/pdf", bytes.len()),
            bytes,
        )
        .await
        .expect("served after the slot freed"),
    );
    assert!(text.contains("Queued, not refused"), "{text:?}");
    assert!(
        started.elapsed() >= Duration::from_millis(250),
        "the request waited for the slot: {:?}",
        started.elapsed()
    );
    release.await.expect("released");
    assert_eq!(served.exhausted.0.load(Ordering::SeqCst), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_slot_wait_is_its_own_bound() {
    // One slot held past the slot wait (150 ms) while the extraction
    // timeout is long (10 s): RESOURCE_EXHAUSTED after the slot wait, and
    // only then.
    let served = serve_with(
        Caps {
            timeout: Duration::from_secs(10),
            ..Caps::default()
        },
        Capacity {
            concurrency: 1,
            slot_wait: Duration::from_millis(150),
            stream_idle: Duration::from_millis(500),
        },
        BTreeSet::from([own_uid()]),
    );
    let _held = served.service.hold_slot().await;
    let bytes = pdf(&["No slot for this one."]);
    let started = std::time::Instant::now();
    let status = client::extract(
        served.channel.clone(),
        header("waiting.pdf", "application/pdf", bytes.len()),
        bytes,
    )
    .await
    .expect_err("refused");
    assert_eq!(status.code(), tonic::Code::ResourceExhausted, "{status}");
    assert!(status.message().contains("150 ms"), "{status}");
    assert!(
        started.elapsed() >= Duration::from_millis(140)
            && started.elapsed() < Duration::from_secs(5),
        "{:?}",
        started.elapsed()
    );
    // Not an overrun: the slot was merely busy.
    assert_eq!(served.exhausted.0.load(Ordering::SeqCst), 0);
    assert_eq!(served.service.overruns(), 0);
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
    // One slot, a 1 ms timeout (the parser gets a whole second, the request
    // waits that plus the grace) and an extraction held for longer than
    // that: the request is answered TIMEOUT after the outer timeout and
    // holds the slot, which is every slot there is. Capabilities (the health
    // probe's answer) reports the wedged state while it lasts.
    let asked = Duration::from_millis(1);
    let served = serve_delayed(
        Caps {
            timeout: asked,
            ..Caps::default()
        },
        capacity(1),
        BTreeSet::from([own_uid()]),
        outer_timeout(asked) + Duration::from_secs(1),
    );
    let bytes = pdf(&["Slow page."]);
    let started = std::time::Instant::now();
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
    assert!(
        started.elapsed() >= outer_timeout(asked),
        "answered before the outer timeout: {:?}",
        started.elapsed()
    );
    assert_eq!(served.exhausted.0.load(Ordering::SeqCst), 1);
    let answer = client::capabilities(served.channel.clone())
        .await
        .expect("served");
    assert_eq!(
        (answer.busy, answer.overruns, answer.concurrency),
        (1, 1, 1)
    );
    assert!(
        !elitea_doc_extractor::service::is_healthy(&answer),
        "every slot held by an overrun is not healthy"
    );
    // The thread ends; the slot and the body come back, health returns.
    let waited = std::time::Instant::now();
    while served.service.overruns() != 0 && waited.elapsed() < Duration::from_secs(10) {
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert_eq!(served.service.overruns(), 0);
    assert_eq!(served.service.held_body_bytes(), 0);
    let answer = client::capabilities(served.channel.clone())
        .await
        .expect("served");
    assert_eq!((answer.busy, answer.overruns), (0, 0));
    assert!(elitea_doc_extractor::service::is_healthy(&answer));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_client_that_cancels_a_hung_extraction_leaves_its_thread_counted() {
    // One slot; the extraction is held for 2 s; the client gives up after
    // 300 ms (drops its call, which resets the stream and drops the
    // server's request future) — long before the request's own 10 s
    // timeout. Without the request's guard the thread would hold the only
    // slot uncounted: no TIMEOUT answer, no overrun, no exhaustion, a
    // process that serves nothing and looks healthy. With it the thread is
    // an overrun from the moment the request is dropped, exhaustion is
    // reported once, and everything is released when the thread ends.
    let served = serve_delayed(
        Caps {
            timeout: Duration::from_secs(10),
            ..Caps::default()
        },
        capacity(1),
        BTreeSet::from([own_uid()]),
        Duration::from_secs(2),
    );
    let bytes = pdf(&["Abandoned page."]);
    let cancelled = tokio::time::timeout(
        Duration::from_millis(300),
        client::extract(
            served.channel.clone(),
            header("abandoned.pdf", "application/pdf", bytes.len()),
            bytes,
        ),
    )
    .await;
    assert!(
        cancelled.is_err(),
        "the client gave up, nothing was answered"
    );
    // The reset reaches the server a moment later.
    let waited = std::time::Instant::now();
    while served.service.overruns() != 1 && waited.elapsed() < Duration::from_secs(1) {
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(served.service.overruns(), 1, "counted at the drop");
    assert_eq!(served.exhausted.0.load(Ordering::SeqCst), 1);
    assert_eq!(served.service.busy(), 1);
    let answer = client::capabilities(served.channel.clone())
        .await
        .expect("the probe is served while wedged");
    assert_eq!((answer.busy, answer.overruns), (1, 1));
    assert!(!elitea_doc_extractor::service::is_healthy(&answer));
    // A second request now queues for the slot (the slot wait is 30 s in
    // this harness) and is served once the thread ends — without holding a
    // body while it waits.
    let queued_bytes = pdf(&["Served after the overrun ended."]);
    let queued = tokio::spawn(client::extract(
        served.channel.clone(),
        header("queued.pdf", "application/pdf", queued_bytes.len()),
        queued_bytes,
    ));
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert_eq!(
        served.service.held_body_bytes(),
        pdf(&["Abandoned page."]).len() as u64,
        "only the abandoned document's body is held; the queued one holds none"
    );
    let text = text_of(queued.await.expect("joined").expect("served"));
    assert!(text.contains("Served after the overrun ended"), "{text:?}");
    assert_eq!(served.service.overruns(), 0);
    assert_eq!(served.service.held_body_bytes(), 0);
    assert_eq!(
        served.exhausted.0.load(Ordering::SeqCst),
        1,
        "reported once"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_queued_request_holds_no_body_bytes_until_it_has_a_slot() {
    // One slot, held. Two requests stream their whole document and queue
    // for the slot: while they wait the process holds NONE of their bytes
    // (the slot is taken before the body is read), so the bodies in memory
    // are bounded by the concurrency, not by what a client has in flight.
    // When the slot frees they are read and served one after the other.
    let served = serve_with(
        Caps::default(),
        Capacity {
            concurrency: 1,
            slot_wait: Duration::from_secs(30),
            stream_idle: Duration::from_secs(5),
        },
        BTreeSet::from([own_uid()]),
    );
    let held = served.service.hold_slot().await;
    let first = pdf(&["First queued document."]);
    let second = pdf(&["Second queued document."]);
    let a = tokio::spawn(client::extract(
        served.channel.clone(),
        header("a.pdf", "application/pdf", first.len()),
        first,
    ));
    let b = tokio::spawn(client::extract(
        served.channel.clone(),
        header("b.pdf", "application/pdf", second.len()),
        second,
    ));
    // Both requests have reached the server and wait for the slot. Sample
    // the held total several times: it stays at zero the whole while.
    for _ in 0..10 {
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(
            served.service.held_body_bytes(),
            0,
            "a queued request holds no body"
        );
        assert_eq!(served.service.busy(), 1, "only the held slot");
    }
    assert!(!a.is_finished() && !b.is_finished(), "both still queued");
    drop(held);
    let texts = [
        text_of(a.await.expect("joined").expect("served")),
        text_of(b.await.expect("joined").expect("served")),
    ];
    assert!(
        texts.iter().any(|t| t.contains("First queued")),
        "{texts:?}"
    );
    assert!(
        texts.iter().any(|t| t.contains("Second queued")),
        "{texts:?}"
    );
    assert_eq!(served.service.held_body_bytes(), 0);
    assert_eq!(served.service.overruns(), 0);
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
            message: Some(extract_request::Message::Chunk(
                prost::bytes::Bytes::from_static(b"%PDF"),
            )),
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

// ── embedded pictures (ADR-0030 I4 part C) ──────────────────────────────────

const PNG_SIGNATURE: &[u8] = b"\x89PNG\r\n\x1a\n";

fn asking(max_images: u32, max_image_bytes: u64, max_total_bytes: u64) -> ExtractImages {
    ExtractImages {
        max_images,
        max_image_bytes,
        max_total_bytes,
    }
}

/// The document's answer, asking for pictures as `images` says.
async fn extracted(
    served: &Served,
    name: &str,
    mime: &str,
    bytes: Vec<u8>,
    images: Option<ExtractImages>,
) -> ExtractedText {
    let mut head = header(name, mime, bytes.len());
    head.images = images;
    match client::extract(served.channel.clone(), head, bytes)
        .await
        .unwrap_or_else(|status| panic!("{name}: {status}"))
        .outcome
    {
        Some(extract_response::Outcome::Text(text)) => text,
        other => panic!("{name}: expected text, got {other:?}"),
    }
}

/// A DOCX with a picture between each pair of sentences.
fn three_pictures(images: &[Vec<u8>]) -> Vec<u8> {
    docx_with(
        &[
            Block::Text("Before the first picture."),
            Block::Picture(""),
            Block::Text("Between pictures."),
            Block::Picture("a chart"),
            Block::Text("Between again."),
            Block::Picture("a third"),
            Block::Text("After the pictures."),
        ],
        images,
    )
}

#[tokio::test(flavor = "multi_thread")]
async fn a_docx_picture_is_anchored_right_after_its_own_line_and_its_bytes_pass_through() {
    let served = serve(Caps::default(), 2);
    let (red, green) = (png([255, 0, 0]), png([0, 255, 0]));
    let bytes = docx_with(
        &[
            Block::Text("Before the first picture."),
            Block::Picture(""),
            Block::Text("Between pictures."),
            Block::Picture("a chart"),
            Block::Text("After the pictures."),
        ],
        &[red.clone(), green.clone()],
    );
    let text = extracted(&served, "pics.docx", DOCX, bytes, Some(asking(0, 0, 0))).await;
    assert_eq!(text.images.len(), 2, "{:?}", text.images_skipped);
    let (first, second) = (&text.images[0], &text.images[1]);
    let before = |image: &elitea_doc_extractor::pb::EmbeddedImage| {
        text.text[..usize::try_from(image.text_offset).expect("offset")].to_owned()
    };
    // The picture's own line is the anchor; the description goes after it.
    assert_eq!(
        before(first),
        "Before the first picture.\n\n[Image: Picture 1]"
    );
    assert_eq!(
        before(second),
        "Before the first picture.\n\n[Image: Picture 1]\n\nBetween pictures.\n\n[Image: a chart]"
    );
    // Bytes are the package's own: nothing was decoded or re-encoded.
    assert_eq!(first.data.as_ref(), red.as_slice());
    assert_eq!(second.data.as_ref(), green.as_slice());
    assert_eq!(
        (first.media_type.as_str(), second.media_type.as_str()),
        ("image/png", "image/png")
    );
    assert_eq!(
        (first.name.as_str(), second.name.as_str()),
        ("image1.png", "image2.png")
    );
    assert!(first.ordinal < second.ordinal);
    assert_eq!(text.images_skipped.unwrap_or_default().too_many, 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn pptx_and_pdf_pictures_are_anchored_at_the_end_of_their_page() {
    let served = serve(Caps::default(), 2);
    let blue = png([0, 0, 255]);
    let slides = pptx_with(
        &[("Slide one", true), ("Slide two", false)],
        std::slice::from_ref(&blue),
    );
    let text = extracted(&served, "deck.pptx", PPTX, slides, Some(asking(0, 0, 0))).await;
    assert_eq!(text.images.len(), 1, "{:?}", text.images_skipped);
    let image = &text.images[0];
    assert_eq!(image.page, 1);
    assert_eq!(
        &text.text[..usize::try_from(image.text_offset).expect("offset")],
        "Slide one\n\nSlide one tail"
    );
    assert_eq!(image.data.as_ref(), blue.as_slice());

    let pdf = pdf_with_pictures(&[("Page one", true), ("Page two", false)]);
    let text = extracted(
        &served,
        "paper.pdf",
        "application/pdf",
        pdf,
        Some(asking(0, 0, 0)),
    )
    .await;
    assert_eq!(text.images.len(), 1, "{:?}", text.images_skipped);
    let image = &text.images[0];
    assert_eq!(image.page, 1);
    assert_eq!(
        &text.text[..usize::try_from(image.text_offset).expect("offset")],
        "Page one before\n\nPage one after"
    );
    // The parser hands a PDF picture over as an image file it wrote.
    assert_eq!(image.media_type, "image/png");
    assert!(image.data.starts_with(PNG_SIGNATURE));
    assert!(image.name.is_empty(), "{:?}", image.name);
}

#[tokio::test(flavor = "multi_thread")]
async fn asking_for_pictures_never_changes_the_text_and_not_asking_reads_none() {
    let served = serve(Caps::default(), 2);
    let cases: [(&str, &str, Vec<u8>); 3] = [
        (
            "a.docx",
            DOCX,
            three_pictures(&[png([1, 2, 3]), png([4, 5, 6]), png([7, 8, 9])]),
        ),
        (
            "b.pptx",
            PPTX,
            pptx_with(
                &[("One", true), ("Two", true)],
                &[png([1, 1, 1]), png([2, 2, 2])],
            ),
        ),
        (
            "c.pdf",
            "application/pdf",
            pdf_with_pictures(&[("One", true), ("Two", true)]),
        ),
    ];
    for (name, mime, bytes) in cases {
        let plain = extracted(&served, name, mime, bytes.clone(), None).await;
        assert!(plain.images.is_empty(), "{name}");
        assert!(
            plain.images_skipped.is_none(),
            "{name}: {:?}",
            plain.images_skipped
        );
        let asked = extracted(&served, name, mime, bytes, Some(asking(0, 0, 0))).await;
        assert!(!asked.images.is_empty(), "{name}");
        assert_eq!(
            asked.text, plain.text,
            "{name}: the text is the same either way"
        );
    }
    // A format without pictures answers an ask with none, not an error.
    let sheet = xlsx(&["Refund window", "thirty days"]);
    let text = extracted(&served, "s.xlsx", XLSX, sheet, Some(asking(0, 0, 0))).await;
    assert!(text.images.is_empty());
    let none = docx(&["No pictures here."]);
    let text = extracted(&served, "n.docx", DOCX, none, Some(asking(0, 0, 0))).await;
    assert!(text.images.is_empty());
    assert_eq!(text.images_skipped.unwrap_or_default().too_many, 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_picture_bounds_count_what_they_drop() {
    let served = serve(Caps::default(), 2);
    let small = png([9, 9, 9]);
    let small_len = small.len() as u64;
    let three = || three_pictures(&[small.clone(), small.clone(), small.clone()]);

    // Too many: the first two are kept, in document order.
    let text = extracted(&served, "m.docx", DOCX, three(), Some(asking(2, 0, 0))).await;
    assert_eq!(text.images.len(), 2);
    assert_eq!(text.images_skipped.unwrap_or_default().too_many, 1);
    assert!(text.images[0].ordinal < text.images[1].ordinal);

    // One picture over the per-picture bound; the others still come.
    let big = big_png([1, 2, 3], 6000);
    let mixed = three_pictures(&[small.clone(), big, small.clone()]);
    let text = extracted(&served, "b.docx", DOCX, mixed, Some(asking(0, 1000, 0))).await;
    assert_eq!(text.images.len(), 2);
    assert_eq!(text.images_skipped.unwrap_or_default().too_large, 1);
    assert!(text.images.iter().all(|image| image.data.len() <= 1000));

    // The total bound: the first fits, the next two would cross it.
    let text = extracted(
        &served,
        "t.docx",
        DOCX,
        three(),
        Some(asking(0, 0, small_len + 10)),
    )
    .await;
    assert_eq!(text.images.len(), 1);
    assert_eq!(text.images_skipped.unwrap_or_default().too_large, 2);

    // A picture no model takes (SVG) and one that is not an image at all.
    let odd = three_pictures(&[
        small.clone(),
        b"<svg xmlns='x'/>".to_vec(),
        b"junk".to_vec(),
    ]);
    let text = extracted(&served, "u.docx", DOCX, odd, Some(asking(0, 0, 0))).await;
    assert_eq!(text.images.len(), 1);
    assert_eq!(text.images_skipped.unwrap_or_default().unsupported, 2);
}

/// A DOCX of `count` pictures, each its own paragraph.
fn many_pictures(count: usize, image: &[u8]) -> Vec<u8> {
    let mut blocks = vec![Block::Text("Before the pictures.")];
    blocks.extend((0..count).map(|_| Block::Picture("")));
    blocks.push(Block::Text("After the pictures."));
    docx_with(&blocks, &vec![image.to_vec(); count])
}

#[tokio::test(flavor = "multi_thread")]
async fn a_package_whose_media_outweigh_the_work_bound_gives_its_text_and_no_picture() {
    let served = serve(Caps::default(), 2);
    let small = png([3, 3, 3]);
    let small_len = small.len() as u64;
    // Work bound: four times the asked count. 8 media for 2 pictures are
    // still read (2 kept, 6 counted); 9 are not read at all.
    let text = extracted(
        &served,
        "eight.docx",
        DOCX,
        many_pictures(8, &small),
        Some(asking(2, 0, 0)),
    )
    .await;
    assert_eq!(text.images.len(), 2);
    assert_eq!(text.images_skipped.unwrap_or_default().too_many, 6);
    let text = extracted(
        &served,
        "nine.docx",
        DOCX,
        many_pictures(9, &small),
        Some(asking(2, 0, 0)),
    )
    .await;
    assert!(text.images.is_empty(), "no picture is read from it");
    assert!(text.text.contains("Before the pictures."), "{}", text.text);
    assert_eq!(text.images_skipped.unwrap_or_default().too_many, 9);

    // The same for the declared bytes: five pictures of `small_len` weigh
    // more than four times a total bound of `small_len`.
    let text = extracted(
        &served,
        "heavy.docx",
        DOCX,
        many_pictures(5, &small),
        Some(asking(0, 0, small_len)),
    )
    .await;
    assert!(text.images.is_empty());
    assert_eq!(text.images_skipped.unwrap_or_default().too_large, 5);

    // PPTX: media under `ppt/media` are counted the same way.
    let slides: Vec<(&str, bool)> = (0..9).map(|_| ("Slide", true)).collect();
    let deck = pptx_with(&slides, &vec![small.clone(); 9]);
    let text = extracted(&served, "deck.pptx", PPTX, deck, Some(asking(2, 0, 0))).await;
    assert!(text.images.is_empty());
    assert_eq!(text.images_skipped.unwrap_or_default().too_many, 9);
    assert!(text.text.contains("Slide"));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_pdf_with_a_picture_on_every_page_returns_the_bound_and_counts_the_rest() {
    let served = serve(Caps::default(), 2);
    let pages: Vec<(&str, bool)> = (0..30).map(|_| ("Page", true)).collect();
    let text = extracted(
        &served,
        "long.pdf",
        "application/pdf",
        pdf_with_pictures(&pages),
        Some(asking(5, 0, 0)),
    )
    .await;
    assert_eq!(text.images.len(), 5);
    let returned: usize = text.images.iter().map(|image| image.data.len()).sum();
    assert!(returned as u64 <= ImageCaps::default().max_total_bytes);
    assert_eq!(text.images_skipped.unwrap_or_default().too_many, 25);
    // Document order: the first five pages.
    let pages: Vec<u32> = text.images.iter().map(|image| image.page).collect();
    assert_eq!(pages, vec![1, 2, 3, 4, 5]);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_request_is_lowered_to_the_extractors_picture_caps_which_capabilities_states() {
    let caps = ImageCaps {
        max_images: 1,
        max_image_bytes: 4000,
        max_total_bytes: 4000,
    };
    let served = serve_full(
        Caps::default(),
        capacity(2),
        BTreeSet::from([own_uid()]),
        Duration::ZERO,
        caps,
    );
    let advertised = client::capabilities(served.channel.clone())
        .await
        .expect("capabilities")
        .image_limits
        .expect("image limits");
    assert_eq!(
        (
            advertised.max_images,
            advertised.max_image_bytes,
            advertised.max_total_bytes
        ),
        (1, 4000, 4000)
    );
    let small = png([5, 5, 5]);
    let two = || three_pictures(&[small.clone(), small.clone(), small.clone()]);
    // Zero asks for the cap; more than the cap is lowered to it.
    for ask in [Some(asking(0, 0, 0)), Some(asking(99, 1 << 30, 1 << 30))] {
        let text = extracted(&served, "c.docx", DOCX, two(), ask).await;
        assert_eq!(text.images.len(), 1);
        assert_eq!(text.images_skipped.unwrap_or_default().too_many, 2);
    }
    // A picture over the extractor's own per-picture cap is dropped even
    // when the request would take it.
    let big = three_pictures(&[big_png([1, 2, 3], 6000)]);
    let text = extracted(&served, "d.docx", DOCX, big, Some(asking(0, 1 << 30, 0))).await;
    assert!(text.images.is_empty());
    assert_eq!(text.images_skipped.unwrap_or_default().too_large, 1);
}
