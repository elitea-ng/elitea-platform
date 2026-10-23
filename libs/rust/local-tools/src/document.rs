//! `read_document`: text from PDF, Office, e-mail and text files, page by
//! page, with the extractor behind a seam (ADR-0029 decision 4).
//!
//! The desktop does not link the extractor (xberg turns on `serde_json`
//! `preserve_order`, which decision 2 keeps out of the host). It hands the
//! file's bytes to a bundled sidecar ([`SidecarExtractor`]) that runs under
//! the OS sandbox: read-only, no network, the session's deny list, an empty
//! environment, and a wall-clock limit that kills the process group. A crash
//! or a timeout is a tool error; the host keeps running.

use std::fmt::Write as _;
use std::io::{Read as _, Write as _};
use std::os::unix::process::CommandExt as _;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use elitea_doc_extract::wire;
pub use elitea_doc_extract::{
    DocumentExtractor, ExtractFailure, InProcessExtractor, Page, PageRange, PagedText,
};
use serde::Deserialize;
use serde_json::{Value, json};

use crate::budget::{self, SpillStore};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::files::{MAX_DOCUMENT_BYTES, MAX_DOCUMENT_OUTPUT};
use crate::sandbox::{self, SandboxConfig, SandboxRequest};
use crate::workspace::{Intent, Workspace};

/// How long the sidecar may run before its process group is killed. The
/// extractor's own cooperative limit is 120 s; this is the backstop for a
/// parse that does not honour it.
pub const SIDECAR_TIMEOUT: Duration = Duration::from_secs(130);
/// The most stdout read from the sidecar.
pub const SIDECAR_MAX_ANSWER: u64 = 8 * 1024 * 1024;

/// Where a session's `read_document` gets its text.
#[derive(Clone, Default)]
pub enum Documents {
    /// In this process: text files, and PDF and Office only when this build
    /// links the extractor (`documents` feature on `elitea-doc-extract`).
    #[default]
    InProcess,
    /// The extractor binary at this path, run under the session's sandbox
    /// for PDF and Office files; text files are read in this process.
    Sidecar(PathBuf),
    /// A host's own extractor, for every file it is asked about.
    Custom(Arc<dyn DocumentExtractor>),
}

impl std::fmt::Debug for Documents {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::InProcess => f.write_str("InProcess"),
            Self::Sidecar(path) => f.debug_tuple("Sidecar").field(path).finish(),
            Self::Custom(_) => f.write_str("Custom"),
        }
    }
}

/// The bundled extractor process, one run per document.
pub struct SidecarExtractor<'a> {
    program: &'a Path,
    request: &'a SandboxRequest,
    config: &'a SandboxConfig,
    timeout: Duration,
}

impl<'a> SidecarExtractor<'a> {
    /// `program` run under `request` (the session's read-only, network-off
    /// sandbox request) and `config`.
    #[must_use]
    pub fn new(program: &'a Path, request: &'a SandboxRequest, config: &'a SandboxConfig) -> Self {
        Self {
            program,
            request,
            config,
            timeout: SIDECAR_TIMEOUT,
        }
    }

    /// The same with another wall-clock limit.
    #[must_use]
    pub fn with_timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    fn run(&self, request: &[u8]) -> Result<Vec<u8>, ExtractFailure> {
        let argv = vec![self.program.display().to_string()];
        let prepared = sandbox::prepare(self.request, &argv, self.config)
            .map_err(|error| ExtractFailure::Unconfined(error.message().to_owned()))?;
        if !prepared.hides_denied {
            // The document is data, but a crafted one must not be able to
            // make the parser read what the session denies; without a
            // sandbox that hides those paths, the document is not parsed.
            return Err(ExtractFailure::Unconfined(
                "the sandbox here cannot hide the credentials the session denies, so documents are not extracted"
                    .to_owned(),
            ));
        }
        let mut command = Command::new(&prepared.program);
        command
            .args(&prepared.args)
            .env_clear()
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .process_group(0);
        let mut child = command.spawn().map_err(|error| {
            ExtractFailure::Failed(format!("cannot start the document extractor: {error}"))
        })?;
        let group = i32::try_from(child.id())
            .ok()
            .and_then(rustix::process::Pid::from_raw);
        let kill_group = || {
            if let Some(group) = group {
                let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
            }
        };
        let (Some(mut stdin), Some(stdout)) = (child.stdin.take(), child.stdout.take()) else {
            kill_group();
            return Err(ExtractFailure::Failed(
                "the document extractor has no pipes".to_owned(),
            ));
        };
        let request = request.to_vec();
        let writer = std::thread::spawn(move || {
            // A sidecar that exits early closes the pipe: the answer says why.
            let _ = stdin.write_all(&request);
        });
        let overflow = Arc::new(AtomicBool::new(false));
        let reader = {
            let overflow = Arc::clone(&overflow);
            std::thread::spawn(move || {
                let mut out = Vec::new();
                let _ = stdout.take(SIDECAR_MAX_ANSWER + 1).read_to_end(&mut out);
                if out.len() as u64 > SIDECAR_MAX_ANSWER {
                    overflow.store(true, Ordering::SeqCst);
                }
                out
            })
        };
        let deadline = Instant::now() + self.timeout;
        let mut timed_out = false;
        loop {
            match child.try_wait() {
                Ok(Some(_)) => break,
                Ok(None) if overflow.load(Ordering::SeqCst) => {
                    kill_group();
                    let _ = child.kill();
                    let _ = child.wait();
                    break;
                }
                Ok(None) if Instant::now() >= deadline => {
                    timed_out = true;
                    kill_group();
                    let _ = child.kill();
                    let _ = child.wait();
                    break;
                }
                Ok(None) => std::thread::sleep(Duration::from_millis(10)),
                Err(_) => {
                    kill_group();
                    let _ = child.kill();
                    break;
                }
            }
        }
        // Anything the sidecar left behind in its group may hold the pipes.
        kill_group();
        let _ = writer.join();
        let out = reader.join().unwrap_or_default();
        if timed_out {
            return Err(ExtractFailure::Timeout(format!(
                "the document extractor ran past {} s and was stopped",
                self.timeout.as_secs()
            )));
        }
        if overflow.load(Ordering::SeqCst) {
            return Err(ExtractFailure::Failed(
                "the document extractor's answer was larger than the limit".to_owned(),
            ));
        }
        Ok(out)
    }
}

#[derive(Deserialize)]
struct Reply {
    ok: bool,
    #[serde(default)]
    extractor: String,
    #[serde(default)]
    page_count: u32,
    #[serde(default)]
    pages: Vec<ReplyPage>,
    #[serde(default)]
    error: String,
    #[serde(default)]
    message: String,
}

#[derive(Deserialize)]
struct ReplyPage {
    n: u32,
    text: String,
}

impl DocumentExtractor for SidecarExtractor<'_> {
    fn extract(
        &self,
        mime: &str,
        bytes: Vec<u8>,
        range: &PageRange,
    ) -> Result<PagedText, ExtractFailure> {
        // Text is read here: nothing to parse, nothing to confine.
        if !elitea_doc_extract::is_document_type(mime) {
            return InProcessExtractor.extract(mime, bytes, range);
        }
        let mut request = wire::encode_request_head(mime, range).map_err(ExtractFailure::Failed)?;
        request.extend_from_slice(&bytes);
        drop(bytes);
        let out = self.run(&request)?;
        let Ok(reply) = serde_json::from_slice::<Reply>(&out) else {
            return Err(ExtractFailure::Unreadable(
                "the document extractor stopped without an answer (the document crashed it, or it ran out of memory)"
                    .to_owned(),
            ));
        };
        if !reply.ok {
            return Err(match reply.error.as_str() {
                "unsupported" => ExtractFailure::Unsupported,
                "timeout" => ExtractFailure::Timeout(reply.message),
                _ => ExtractFailure::Unreadable(reply.message),
            });
        }
        Ok(PagedText {
            extractor: reply.extractor,
            page_count: reply.page_count,
            pages: reply
                .pages
                .into_iter()
                .map(|page| Page {
                    number: page.n,
                    text: page.text,
                })
                .collect(),
        })
    }
}

#[derive(Deserialize)]
struct DocumentArgs {
    path: String,
    #[serde(default)]
    pages: Option<String>,
    #[serde(default)]
    offset: Option<usize>,
}

/// `read_document`: the pages asked for (all, by default) as one text, the
/// part of it that fits `max_bytes` from `offset` on, and where to continue.
///
/// Pages of a paged document are marked `[page N]`. Past `max_bytes` the head
/// is shown and the rest spilled for `read_output`; `next_offset` continues
/// inside the selection, `next_pages` names the pages the selection did not
/// reach (it is capped at [`MAX_DOCUMENT_OUTPUT`] bytes of text).
pub(crate) fn read(
    workspace: &Workspace,
    args: Value,
    spill: &SpillStore,
    max_bytes: usize,
    extractor: &dyn DocumentExtractor,
) -> ToolResult<Value> {
    let args: DocumentArgs = serde_json::from_value(args)
        .map_err(|error| ToolError::invalid(format!("bad arguments: {error}")))?;
    let range = match &args.pages {
        Some(pages) => PageRange::parse(pages).map_err(ToolError::invalid)?,
        None => PageRange::ALL,
    };
    let path = workspace.resolve(&args.path, Intent::Read)?;
    let file = workspace.read(&path, MAX_DOCUMENT_BYTES)?;
    let name = file.path.file_name().unwrap_or_default();
    let mime = elitea_content_source::mime_of(name);
    let shown_path = file.path.display_string();
    let paged = extractor
        .extract(mime, file.bytes, &range)
        .map_err(|failure| match failure {
            ExtractFailure::Unsupported => ToolError::new(
                ErrorCode::Unsupported,
                format!("this build cannot extract text from {mime}"),
            ),
            ExtractFailure::Unreadable(reason) => ToolError::new(
                ErrorCode::Binary,
                format!("`{path}` is unreadable as {mime}: {reason}"),
            ),
            ExtractFailure::Timeout(reason) => ToolError::new(ErrorCode::Timeout, reason),
            ExtractFailure::Failed(reason) => ToolError::new(ErrorCode::Io, reason),
            ExtractFailure::Unconfined(reason) => {
                ToolError::new(ErrorCode::SandboxUnavailable, reason)
            }
        })?;
    if paged.pages.is_empty() && range.from > paged.page_count.max(1) {
        return Err(ToolError::invalid(format!(
            "`{path}` has {} page(s); pages {range} is past the end",
            paged.page_count.max(1)
        )));
    }
    let selection = Selection::of(&paged);
    let tail_from = selection.byte_of_char(args.offset.unwrap_or(0));
    let tail = &selection.text[tail_from..];
    let consumed = budget::shown_len(tail, max_bytes);
    let (shown, output_id, cut) = budget::spill_over(spill, tail, max_bytes);
    let shown_end = tail_from + consumed;
    let (first, last) = selection.pages_between(tail_from, shown_end);
    let more_here = cut.then(|| selection.char_of_byte(shown_end));
    let end = range
        .to
        .map_or(paged.page_count, |to| to.min(paged.page_count));
    let resume = selection.resume_page().filter(|page| *page <= end);
    let mut answer = json!({
        "path": shown_path,
        "mime_type": mime,
        "extractor": paged.extractor,
        "page_count": paged.page_count.max(1),
        "text": shown,
        "truncated": cut || resume.is_some(),
        "bytes": selection.text.len(),
    });
    if let Value::Object(map) = &mut answer {
        if let (Some(first), Some(last)) = (first, last) {
            map.insert(
                "pages".to_owned(),
                Value::String(
                    PageRange {
                        from: first,
                        to: Some(last),
                    }
                    .to_string(),
                ),
            );
        }
        if let Some(offset) = more_here {
            map.insert("next_offset".to_owned(), json!(offset));
        }
        if let Some(from) = resume {
            let next = PageRange { from, to: range.to };
            map.insert("next_pages".to_owned(), Value::String(next.to_string()));
        }
        if let Some(id) = output_id {
            map.insert("output_id".to_owned(), Value::String(id));
        }
    }
    Ok(answer)
}

/// The selected pages as one text, with where each page starts in it.
struct Selection {
    text: String,
    /// Each page's number and the byte its marker starts at.
    starts: Vec<(u32, usize)>,
    /// The page the cap cut inside, if any.
    cut_page: Option<u32>,
    /// The last page entirely in the text, if any.
    last_whole: Option<u32>,
    /// The first page returned, for a selection that starts after page 1.
    first_page: u32,
}

impl Selection {
    fn of(paged: &PagedText) -> Self {
        let marked = paged.page_count > 1;
        let mut text = String::new();
        let mut starts = Vec::new();
        for page in &paged.pages {
            starts.push((page.number, text.len()));
            if marked {
                let _ = writeln!(text, "[page {}]", page.number);
            }
            text.push_str(&page.text);
            if marked {
                text.push_str("\n\n");
            }
            if text.len() >= MAX_DOCUMENT_OUTPUT {
                break;
            }
        }
        let mut cut_page = None;
        if text.len() > MAX_DOCUMENT_OUTPUT {
            let kept = budget::head(&text, MAX_DOCUMENT_OUTPUT).len();
            text.truncate(kept);
            cut_page = starts
                .iter()
                .rev()
                .find(|(_, start)| *start < kept)
                .map(|(number, _)| *number);
        }
        starts.retain(|(_, start)| *start < text.len() || text.is_empty());
        let last_whole = starts
            .iter()
            .map(|(number, _)| *number)
            .filter(|number| Some(*number) != cut_page)
            .max();
        Self {
            first_page: starts.first().map_or(1, |(number, _)| *number),
            text,
            starts,
            cut_page,
            last_whole,
        }
    }

    /// The page the selection stopped before, when it did not reach the
    /// range's end because of the cap or the sidecar's answer limit.
    fn resume_page(&self) -> Option<u32> {
        match (self.cut_page, self.last_whole) {
            (Some(page), _) => Some(page),
            (None, Some(last)) => Some(last + 1),
            (None, None) => None,
        }
    }

    fn byte_of_char(&self, chars: usize) -> usize {
        self.text
            .char_indices()
            .nth(chars)
            .map_or(self.text.len(), |(byte, _)| byte)
    }

    fn char_of_byte(&self, byte: usize) -> usize {
        self.text[..byte.min(self.text.len())].chars().count()
    }

    /// The first and last page that the text between two byte positions
    /// touches.
    fn pages_between(&self, from: usize, to: usize) -> (Option<u32>, Option<u32>) {
        if self.starts.is_empty() {
            return (None, None);
        }
        let page_at = |byte: usize| {
            self.starts
                .iter()
                .rev()
                .find(|(_, start)| *start <= byte)
                .map_or(self.first_page, |(number, _)| *number)
        };
        (
            Some(page_at(from)),
            Some(page_at(to.saturating_sub(1).max(from))),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A document of `count` pages of `size` bytes each, whatever the range
    /// asked for; only `served` pages are handed over (a sidecar that cut its
    /// answer).
    struct Paged {
        count: u32,
        size: usize,
        served: u32,
    }

    impl DocumentExtractor for Paged {
        fn extract(
            &self,
            _mime: &str,
            _bytes: Vec<u8>,
            range: &PageRange,
        ) -> Result<PagedText, ExtractFailure> {
            Ok(PagedText {
                extractor: "fake".to_owned(),
                page_count: self.count,
                pages: (1..=self.served.min(self.count))
                    .filter(|n| range.contains(*n))
                    .map(|n| Page {
                        number: n,
                        text: format!("{n}").repeat(self.size),
                    })
                    .collect(),
            })
        }
    }

    fn call(
        dir: &tempfile::TempDir,
        extractor: &dyn DocumentExtractor,
        args: Value,
        max_bytes: usize,
    ) -> ToolResult<Value> {
        let ws = Workspace::open(dir.path(), &[]).expect("workspace");
        let spill = SpillStore::new(&dir.path().join("..").join("spill"), "t");
        read(&ws, args, &spill, max_bytes, extractor)
    }

    fn setup() -> tempfile::TempDir {
        let dir = tempfile::tempdir().expect("dir");
        std::fs::write(dir.path().join("book.pdf"), b"%PDF-1.4").expect("pdf");
        dir
    }

    #[test]
    fn read_document_pages_and_offset_page_through_text() {
        let dir = setup();
        let fake = Paged {
            count: 6,
            size: 1500,
            served: 6,
        };
        let budget = 2600;
        let first = call(&dir, &fake, json!({ "path": "book.pdf" }), budget).expect("first");
        assert_eq!(first["page_count"], 6);
        assert_eq!(first["pages"], "1-2", "{first}");
        assert!(
            first["text"]
                .as_str()
                .expect("text")
                .starts_with("[page 1]\n1111")
        );
        assert_eq!(first["truncated"], true);
        let mut offset = first["next_offset"].as_u64().expect("next_offset");
        assert!(
            first.get("next_pages").is_none(),
            "the selection holds every page"
        );

        // Following next_offset reaches every page, in order, once.
        let mut seen = vec![first["pages"].as_str().expect("pages").to_owned()];
        let mut guard = 0;
        loop {
            let next = call(
                &dir,
                &fake,
                json!({ "path": "book.pdf", "offset": offset }),
                budget,
            )
            .expect("next");
            seen.push(next["pages"].as_str().unwrap_or("").to_owned());
            match next["next_offset"].as_u64() {
                Some(more) => offset = more,
                None => break,
            }
            guard += 1;
            assert!(guard < 20, "paging does not end");
        }
        assert_eq!(seen.last().map(String::as_str), Some("6"), "{seen:?}");

        // A page range reads just those pages.
        let range = call(
            &dir,
            &fake,
            json!({ "path": "book.pdf", "pages": "3-4" }),
            1 << 20,
        )
        .expect("range");
        assert_eq!(range["pages"], "3-4");
        let text = range["text"].as_str().expect("text");
        assert!(text.starts_with("[page 3]\n3333"), "{text}");
        assert!(text.contains("[page 4]\n4444") && !text.contains("[page 5]"));
        assert_eq!(range["truncated"], false);
        assert!(range.get("next_pages").is_none());

        // An open range keeps its end in next_pages.
        let open = call(
            &dir,
            &fake,
            json!({ "path": "book.pdf", "pages": "5-" }),
            1 << 20,
        )
        .expect("open");
        assert_eq!(open["pages"], "5-6");

        // Past the end is a mistake the model can correct.
        let past = call(
            &dir,
            &fake,
            json!({ "path": "book.pdf", "pages": "9" }),
            1 << 20,
        )
        .expect_err("past the end");
        assert_eq!(past.code(), ErrorCode::InvalidArgument);
        assert!(past.message().contains("6 page"), "{}", past.message());
        let bad = call(
            &dir,
            &fake,
            json!({ "path": "book.pdf", "pages": "0" }),
            1 << 20,
        )
        .expect_err("bad range");
        assert_eq!(bad.code(), ErrorCode::InvalidArgument);
    }

    #[test]
    fn pages_the_extractor_did_not_hand_over_are_named_in_next_pages() {
        let dir = setup();
        // The sidecar cut its answer after three of ten pages.
        let fake = Paged {
            count: 10,
            size: 100,
            served: 3,
        };
        let all = call(&dir, &fake, json!({ "path": "book.pdf" }), 1 << 20).expect("all");
        assert_eq!(all["pages"], "1-3");
        assert_eq!(all["next_pages"], "4-");
        assert_eq!(all["truncated"], true);
        let bounded = call(
            &dir,
            &fake,
            json!({ "path": "book.pdf", "pages": "1-8" }),
            1 << 20,
        )
        .expect("bounded");
        assert_eq!(bounded["next_pages"], "4-8");
    }

    #[test]
    fn a_selection_over_the_output_cap_resumes_at_the_page_it_cut() {
        let dir = setup();
        let fake = Paged {
            count: 5,
            size: 200_000,
            served: 5,
        };
        let result = call(&dir, &fake, json!({ "path": "book.pdf" }), 1 << 22).expect("read");
        let text = result["text"].as_str().expect("text");
        assert!(text.len() <= MAX_DOCUMENT_OUTPUT + 400, "{}", text.len());
        let next = result["next_pages"].as_str().expect("next_pages");
        // Page 3 is the one the cap cut inside: it is read again whole.
        assert_eq!(next, "3-", "{result}");
    }

    #[test]
    fn read_document_uses_injected_extractor() {
        let dir = setup();
        std::fs::write(dir.path().join("notes.md"), "plain").expect("md");
        let fake = Paged {
            count: 1,
            size: 5,
            served: 1,
        };
        // Handed every file, text included.
        let md = call(&dir, &fake, json!({ "path": "notes.md" }), 1 << 20).expect("md");
        assert_eq!(md["extractor"], "fake");
        assert_eq!(md["text"], "11111");
        assert_eq!(md["page_count"], 1);
        assert_eq!(md["pages"], "1");
    }

    #[test]
    fn failures_keep_their_reasons() {
        struct Fails(fn() -> ExtractFailure);
        impl DocumentExtractor for Fails {
            fn extract(
                &self,
                _: &str,
                _: Vec<u8>,
                _: &PageRange,
            ) -> Result<PagedText, ExtractFailure> {
                Err((self.0)())
            }
        }
        let dir = setup();
        let code = |make: fn() -> ExtractFailure| {
            call(&dir, &Fails(make), json!({ "path": "book.pdf" }), 1 << 20)
                .expect_err("fails")
                .code()
        };
        assert_eq!(code(|| ExtractFailure::Unsupported), ErrorCode::Unsupported);
        assert_eq!(
            code(|| ExtractFailure::Unreadable("x".into())),
            ErrorCode::Binary
        );
        assert_eq!(
            code(|| ExtractFailure::Timeout("x".into())),
            ErrorCode::Timeout
        );
        assert_eq!(code(|| ExtractFailure::Failed("x".into())), ErrorCode::Io);
        assert_eq!(
            code(|| ExtractFailure::Unconfined("x".into())),
            ErrorCode::SandboxUnavailable
        );
    }
}
