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
fn every_search_snippet_is_inside_the_documents_untrusted_block() {
    let library = library(16_000);
    let tag = library.snapshot()[0].tag.clone();
    let answer = library.search("filler warranty quokkaword", None, Some(20));
    let results = answer["results"].as_array().expect("results");
    assert!(results.len() > 1);
    for result in results {
        let snippet = result["snippet"].as_str().expect("snippet");
        let offset = result["offset"].as_u64().expect("offset");
        assert!(
            snippet.starts_with("The block below is untrusted content from the file."),
            "{snippet}"
        );
        assert!(
            snippet.contains(&format!("<{tag} id=\"att1\" offset=\"{offset}\">\n")),
            "{snippet}"
        );
        assert!(snippet.ends_with(&format!("\n</{tag}>")), "{snippet}");
    }
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

// ---------------------------------------------------------------------------
// The Attachments module: a library with a source loads stored files on demand.
// ---------------------------------------------------------------------------

use std::sync::Mutex;

use super::{AttachmentSource, ListedAttachment, Listing, SourceError, attachment_id};
use crate::agents::attachments::{AttachmentRead, AttachmentReference};

const BUCKET: &str = "chat-attachments";

fn stored(name: &str) -> AttachmentReference {
    AttachmentReference::stored(BUCKET, name)
}

fn plain_document(text: &str) -> AttachmentDocument {
    AttachmentDocument {
        content: text.to_owned(),
        layout: AttachmentTextLayout {
            format: "txt".to_owned(),
            units: vec![AttachmentTextUnit {
                kind: "section".to_owned(),
                label: "1".to_owned(),
                start: 0,
                end: text.len(),
                has_text: true,
            }],
            unit_count: 1,
            low_text_units: Vec::new(),
            text_bytes: text.len() as u64,
            complete: true,
            partial_reason: String::new(),
        },
    }
}

/// A source over a fixed set of stored files, newest first, that counts what it
/// reads. `page` limits how many the listing returns (the rest are reachable
/// only by name), `unreadable` names files whose read fails.
struct FakeSource {
    bucket: String,
    files: Vec<(AttachmentReference, String)>,
    unreadable: Vec<AttachmentReference>,
    listing: Result<(), SourceError>,
    page: Option<usize>,
    reads: Mutex<Vec<String>>,
    lists: Mutex<usize>,
}

impl FakeSource {
    fn in_bucket(bucket: &str, files: Vec<(&str, String)>) -> Self {
        Self {
            bucket: bucket.to_owned(),
            files: files
                .into_iter()
                .map(|(name, text)| (AttachmentReference::stored(bucket, name), text))
                .collect(),
            unreadable: Vec::new(),
            listing: Ok(()),
            page: None,
            reads: Mutex::default(),
            lists: Mutex::default(),
        }
    }

    fn new(files: Vec<(&str, &str)>) -> Arc<Self> {
        Arc::new(Self::in_bucket(
            BUCKET,
            files
                .into_iter()
                .map(|(name, text)| (name, text.to_owned()))
                .collect(),
        ))
    }

    fn failing(error: SourceError) -> Arc<Self> {
        let mut source = Self::in_bucket(BUCKET, Vec::new());
        source.listing = Err(error);
        Arc::new(source)
    }
}

#[async_trait]
impl AttachmentSource for FakeSource {
    async fn list(&self) -> Result<Listing, SourceError> {
        *self.lists.lock().expect("lists") += 1;
        self.listing?;
        let shown = self.page.unwrap_or(self.files.len());
        Ok(Listing {
            attachments: self
                .files
                .iter()
                .take(shown)
                .enumerate()
                .map(|(index, (reference, text))| ListedAttachment {
                    reference: reference.clone(),
                    size_bytes: text.len() as u64,
                    // The first file is the newest.
                    modified_at: {
                        let second = 86_399 - index.min(86_399);
                        format!(
                            "2026-10-01T{:02}:{:02}:{:02}Z",
                            second / 3_600,
                            second / 60 % 60,
                            second % 60
                        )
                    },
                })
                .collect(),
            truncated: shown < self.files.len(),
            bucket: Some(self.bucket.clone()),
            key_prefix: Some("conv/".to_owned()),
        })
    }

    async fn read(&self, reference: &AttachmentReference) -> AttachmentRead {
        self.reads.lock().expect("reads").push(format!(
            "{}:{}",
            reference.bucket(),
            reference.name()
        ));
        if self.unreadable.contains(reference) {
            return AttachmentRead::Unreadable("encrypted");
        }
        self.files
            .iter()
            .find(|(candidate, _)| candidate == reference)
            .map_or(AttachmentRead::Unreadable("not_found"), |(_, text)| {
                AttachmentRead::Document(plain_document(text))
            })
    }
}

fn sourced(source: Arc<FakeSource>, current: &[AttachmentReference]) -> Arc<AttachmentLibrary> {
    let library = Arc::new(AttachmentLibrary::new(Vec::new(), 4_000));
    library.attach_source(source, current);
    library
}

fn ctx() -> Arc<dyn ToolContext> {
    Arc::new(adk_rust::tool::SimpleToolContext::new("attachments-test"))
}

async fn tool_named(library: &Arc<AttachmentLibrary>, name: &str) -> Arc<dyn Tool> {
    library
        .toolset()
        .tools(Arc::new(adk_rust::tool::SimpleToolContext::new(
            "attachments-test",
        )))
        .await
        .expect("attachment tools")
        .into_iter()
        .find(|tool| tool.name() == name)
        .expect("attachment tool")
}

fn read_count(source: &FakeSource) -> usize {
    source.reads.lock().expect("reads").len()
}

#[tokio::test(flavor = "current_thread")]
async fn a_library_without_a_source_binds_two_tools_and_with_one_binds_three() {
    let names = |library: &Arc<AttachmentLibrary>| {
        let library = Arc::clone(library);
        async move {
            library
                .toolset()
                .tools(Arc::new(adk_rust::tool::SimpleToolContext::new("t")))
                .await
                .expect("tools")
                .iter()
                .map(|tool| tool.name().to_owned())
                .collect::<Vec<_>>()
        }
    };
    let plain = Arc::new(AttachmentLibrary::new(Vec::new(), 4_000));
    assert_eq!(
        names(&plain).await,
        ["read_attachment", "search_attachment"]
    );
    let with_source = sourced(FakeSource::new(vec![]), &[]);
    assert_eq!(
        names(&with_source).await,
        ["list_attachments", "read_attachment", "search_attachment"]
    );
    assert!(with_source.has_source() && !plain.has_source());
}

#[tokio::test(flavor = "current_thread")]
async fn list_attachments_is_newest_first_and_flags_the_current_message() {
    let source = FakeSource::new(vec![
        ("conv/new.txt", "this message's file"),
        ("conv/old.txt", "an earlier file"),
    ]);
    let library = sourced(Arc::clone(&source), &[stored("conv/new.txt")]);
    let list = tool_named(&library, "list_attachments").await;
    let answer = list.execute(ctx(), json!({})).await.expect("listing");
    assert_eq!(answer["count"], 2);
    assert_eq!(answer["order"], "newest upload first");
    assert_eq!(answer["attachments"][0]["name"], "new.txt");
    assert_eq!(answer["attachments"][1]["name"], "old.txt");
    assert_eq!(answer["attachments"][0]["in_this_message"], true);
    assert_eq!(answer["attachments"][1]["in_this_message"], false);
    assert_eq!(answer["attachments"][1]["size_bytes"], 15);
    assert_eq!(answer["attachments"][1]["loaded"], false);
    assert!(answer.get("truncated").is_none());
    // Listed once per run, however often it is asked.
    list.execute(ctx(), json!({})).await.expect("listing");
    assert_eq!(*source.lists.lock().expect("lists"), 1);
    assert_eq!(read_count(&source), 0, "listing reads nothing");
}

#[tokio::test(flavor = "current_thread")]
async fn a_file_is_read_the_first_time_a_tool_names_it_and_not_again() {
    let source = FakeSource::new(vec![("conv/old.txt", "The launch word is QUOKKAWORD.")]);
    let library = sourced(Arc::clone(&source), &[]);
    let id = attachment_id(&stored("conv/old.txt"));
    let read = tool_named(&library, "read_attachment").await;
    for _ in 0..2 {
        let answer = read
            .execute(ctx(), json!({"attachment_id": id}))
            .await
            .expect("read");
        assert!(
            answer["content"]
                .as_str()
                .expect("content")
                .contains("QUOKKAWORD")
        );
    }
    assert_eq!(
        source.reads.lock().expect("reads").as_slice(),
        ["chat-attachments:conv/old.txt"]
    );
    // An id that is neither loaded nor listed is the library's own answer.
    let unknown = read
        .execute(ctx(), json!({"attachment_id": "att-00000000"}))
        .await
        .expect("read");
    assert_eq!(unknown["error"], "attachment_not_found");
    assert_eq!(unknown["available_attachment_ids"], json!([id]));
}

#[tokio::test(flavor = "current_thread")]
async fn an_unreadable_stored_file_is_named_with_its_reason_and_not_read_again() {
    let mut source = FakeSource::in_bucket(BUCKET, vec![("conv/locked.pdf", "x".to_owned())]);
    source.unreadable.push(stored("conv/locked.pdf"));
    let source = Arc::new(source);
    let library = sourced(Arc::clone(&source), &[]);
    let read = tool_named(&library, "read_attachment").await;
    let id = attachment_id(&stored("conv/locked.pdf"));
    for _ in 0..2 {
        let answer = read
            .execute(ctx(), json!({"attachment_id": id}))
            .await
            .expect("read");
        assert_eq!(answer["error"], "attachment_unreadable");
        assert_eq!(answer["reason"], "encrypted");
        assert_eq!(answer["name"], "locked.pdf");
    }
    assert_eq!(
        read_count(&source),
        1,
        "a failure is remembered for the run"
    );
}

#[tokio::test(flavor = "current_thread")]
async fn a_missing_stored_file_is_attachment_not_found_not_a_refusal() {
    // The listing names it but the read finds nothing: main said not found.
    let mut source = FakeSource::in_bucket(BUCKET, vec![("conv/gone.txt", "x".to_owned())]);
    source.files.clear();
    source
        .files
        .push((stored("conv/other.txt"), "y".to_owned()));
    let source = Arc::new(source);
    let library = sourced(source, &[stored("conv/gone.txt")]);
    let answer = tool_named(&library, "read_attachment")
        .await
        .execute(
            ctx(),
            json!({"attachment_id": attachment_id(&stored("conv/gone.txt"))}),
        )
        .await
        .expect("read");
    assert_eq!(answer["error"], "attachment_not_found");
}

fn lettered(files: &[(String, bool)]) -> (Arc<FakeSource>, Vec<String>) {
    let mut source = FakeSource::in_bucket(
        BUCKET,
        files
            .iter()
            .map(|(name, _)| (name.as_str(), "alpha text".to_owned()))
            .collect(),
    );
    for (name, readable) in files {
        if !readable {
            source.unreadable.push(stored(name));
        }
    }
    let names = files.iter().map(|(name, _)| name.clone()).collect();
    (Arc::new(source), names)
}

/// A search window that meets files it cannot read must not meet them again:
/// the next search takes the next files. The unreadable are named once.
#[tokio::test(flavor = "current_thread")]
async fn unreadable_files_never_starve_a_later_searchs_window_and_are_named_once() {
    // Newest first: 8 unreadable, then 10 readable.
    let files: Vec<(String, bool)> = (0..18)
        .map(|index| (format!("conv/f{index:02}.txt"), index >= 8))
        .collect();
    let (source, _) = lettered(&files);
    let library = sourced(Arc::clone(&source), &[]);
    let search = tool_named(&library, "search_attachment").await;

    let first = search
        .execute(ctx(), json!({"query": "alpha"}))
        .await
        .expect("search");
    assert_eq!(
        first["unreadable_attachments"]
            .as_array()
            .expect("named")
            .len(),
        8
    );
    assert!(first["results"].as_array().expect("results").is_empty());
    assert_eq!(read_count(&source), 8);

    let second = search
        .execute(ctx(), json!({"query": "alpha"}))
        .await
        .expect("search");
    assert!(
        second.get("unreadable_attachments").is_none(),
        "named once, not again"
    );
    assert_eq!(second["results"].as_array().expect("results").len(), 8);
    assert_eq!(
        read_count(&source),
        16,
        "the second window read the next 8, not the 8 that failed"
    );
    assert!(
        second["not_searched"]
            .as_str()
            .is_some_and(|note| note.starts_with("2 more attachment(s)"))
    );
}

/// Eight unreadable OLD files and readable newer ones: the newest-first window
/// never even touches the old ones.
#[tokio::test(flavor = "current_thread")]
async fn readable_newer_files_are_searched_ahead_of_unreadable_old_ones() {
    let files: Vec<(String, bool)> = (0..16)
        .map(|index| (format!("conv/f{index:02}.txt"), index < 8))
        .collect();
    let (source, _) = lettered(&files);
    let library = sourced(Arc::clone(&source), &[]);
    let answer = tool_named(&library, "search_attachment")
        .await
        .execute(ctx(), json!({"query": "alpha"}))
        .await
        .expect("search");
    assert_eq!(answer["results"].as_array().expect("results").len(), 8);
    assert!(answer.get("unreadable_attachments").is_none());
    assert_eq!(read_count(&source), 8);
}

/// A listing that fails during a search says so, beside the results from what
/// was already loaded.
#[tokio::test(flavor = "current_thread")]
async fn a_listing_failure_during_search_is_reported_beside_the_loaded_results() {
    let library = Arc::new(AttachmentLibrary::new(
        vec![LibraryDocument::new(
            "att-loaded".to_owned(),
            "loaded.txt".to_owned(),
            plain_document("alpha already here"),
        )],
        4_000,
    ));
    library.attach_source(FakeSource::failing(SourceError::Unavailable), &[]);
    let answer = tool_named(&library, "search_attachment")
        .await
        .execute(ctx(), json!({"query": "alpha"}))
        .await
        .expect("search");
    assert_eq!(answer["results"].as_array().expect("results").len(), 1);
    assert_eq!(answer["listing"]["error"], "attachments_unavailable");
    assert_eq!(answer["listing"]["retry"], true);
}

/// A page that left files out says so, and an older file past the page is still
/// reachable by name, in the bucket the listing named.
#[tokio::test(flavor = "current_thread")]
async fn a_file_past_the_page_is_reachable_by_name_in_the_listed_bucket() {
    let mut source = FakeSource::in_bucket(
        "team-files",
        (0..10)
            .map(|index| {
                (
                    Box::leak(format!("conv/f{index}.txt").into_boxed_str()) as &str,
                    "text".to_owned(),
                )
            })
            .collect(),
    );
    source.page = Some(3);
    let source = Arc::new(source);
    let library = sourced(Arc::clone(&source), &[]);
    let list = tool_named(&library, "list_attachments").await;
    let answer = list.execute(ctx(), json!({})).await.expect("listing");
    assert_eq!(answer["count"], 3);
    assert_eq!(answer["truncated"], true);
    assert!(
        answer["more_not_listed"]
            .as_str()
            .expect("line")
            .contains("newest page")
    );

    let read = tool_named(&library, "read_attachment").await;
    let by_name = read
        .execute(ctx(), json!({"name": "f9.txt"}))
        .await
        .expect("read");
    assert!(by_name["content"].as_str().is_some(), "{by_name}");
    assert_eq!(
        source.reads.lock().expect("reads").as_slice(),
        ["team-files:conv/f9.txt"],
        "the listed bucket, not an assumed one"
    );
    let unknown = read
        .execute(ctx(), json!({"name": "nope.txt"}))
        .await
        .expect("read");
    assert_eq!(unknown["error"], "attachment_not_found");
    let neither = read.execute(ctx(), json!({})).await.expect("read");
    assert_eq!(neither["error"], "attachment_id or name is required");
}

/// This message's own files are readable by id with no listing at all.
#[tokio::test(flavor = "current_thread")]
async fn this_messages_files_are_found_by_id_without_the_listing() {
    let source = FakeSource::new(vec![("conv/mine.txt", "mine")]);
    let library = sourced(Arc::clone(&source), &[stored("conv/mine.txt")]);
    let answer = tool_named(&library, "read_attachment")
        .await
        .execute(
            ctx(),
            json!({"attachment_id": attachment_id(&stored("conv/mine.txt"))}),
        )
        .await
        .expect("read");
    assert!(answer["content"].as_str().is_some());
    assert_eq!(*source.lists.lock().expect("lists"), 0);
}

/// A name lookup needs the listing for the bucket; when it cannot be had the
/// answer is "unavailable, retry", never a guess.
#[tokio::test(flavor = "current_thread")]
async fn a_name_lookup_with_no_listing_asks_for_a_retry() {
    let library = sourced(FakeSource::failing(SourceError::Unavailable), &[]);
    let answer = tool_named(&library, "read_attachment")
        .await
        .execute(ctx(), json!({"name": "report.txt"}))
        .await
        .expect("read");
    assert_eq!(answer["error"], "attachments_unavailable");
    assert_eq!(answer["retry"], true);
}

/// A listing that fails is "try again", never "no such attachment".
#[tokio::test(flavor = "current_thread")]
async fn a_failed_listing_during_a_read_or_search_asks_for_a_retry() {
    for error in [SourceError::Unavailable, SourceError::NotAvailable] {
        let library = sourced(FakeSource::failing(error), &[]);
        let id = attachment_id(&stored("conv/earlier.txt"));
        for (tool, args) in [
            ("read_attachment", json!({"attachment_id": id})),
            (
                "search_attachment",
                json!({"query": "x", "attachment_id": id}),
            ),
        ] {
            let answer = tool_named(&library, tool)
                .await
                .execute(ctx(), args)
                .await
                .expect("answer");
            let expected = match error {
                SourceError::Unavailable => "attachments_unavailable",
                SourceError::NotAvailable => "attachments_not_available",
            };
            assert_eq!(answer["error"], expected, "{tool}");
            assert_ne!(answer["error"], "attachment_not_found");
            if error == SourceError::Unavailable {
                assert_eq!(answer["retry"], true);
            }
        }
    }
}

#[tokio::test(flavor = "current_thread")]
async fn a_listing_the_platform_refuses_is_an_answer_not_a_failure() {
    let library = sourced(FakeSource::failing(SourceError::NotAvailable), &[]);
    let list = tool_named(&library, "list_attachments").await;
    let answer = list.execute(ctx(), json!({})).await.expect("answer");
    assert_eq!(answer["error"], "attachments_not_available");
}

/// The byte cap is enforced on what was ACTUALLY loaded: a document that would
/// pass it is refused with a sentence, kept out of the library and not retried.
#[tokio::test(flavor = "current_thread")]
async fn the_byte_cap_is_enforced_on_the_loaded_size() {
    let nine_mib = "a".repeat(9 * 1_024 * 1_024);
    let source = Arc::new(FakeSource::in_bucket(
        BUCKET,
        vec![
            ("conv/a.txt", nine_mib.clone()),
            ("conv/b.txt", nine_mib),
            ("conv/c.txt", "small".to_owned()),
        ],
    ));
    let library = sourced(Arc::clone(&source), &[]);
    let read = tool_named(&library, "read_attachment").await;
    let id = |name: &str| attachment_id(&stored(name));
    let first = read
        .execute(ctx(), json!({"attachment_id": id("conv/a.txt")}))
        .await
        .expect("read");
    assert!(first["content"].as_str().is_some());
    for _ in 0..2 {
        let second = read
            .execute(ctx(), json!({"attachment_id": id("conv/b.txt")}))
            .await
            .expect("read");
        assert_eq!(second["error"], "attachment_not_loaded");
        assert_eq!(second["reason"], "memory_limit");
        assert!(second["hint"].as_str().expect("sentence").contains("limit"));
    }
    assert_eq!(read_count(&source), 2, "the refused document is read once");
    // A document that still fits loads.
    let third = read
        .execute(ctx(), json!({"attachment_id": id("conv/c.txt")}))
        .await
        .expect("read");
    assert!(third["content"].as_str().is_some());
    assert_eq!(library.ids().len(), 2, "the refused one is not held");
}

/// Two calls for one file at once read it once and hold it once.
#[tokio::test(flavor = "current_thread")]
async fn concurrent_loads_of_one_file_read_and_hold_it_once() {
    let source = FakeSource::new(vec![("conv/same.txt", "shared text")]);
    let library = sourced(Arc::clone(&source), &[]);
    let read = tool_named(&library, "read_attachment").await;
    let id = attachment_id(&stored("conv/same.txt"));
    let (a, b, c) = tokio::join!(
        read.execute(ctx(), json!({"attachment_id": id})),
        read.execute(ctx(), json!({"attachment_id": id})),
        read.execute(ctx(), json!({"attachment_id": id})),
    );
    for answer in [a, b, c] {
        assert!(answer.expect("read")["content"].as_str().is_some());
    }
    assert_eq!(read_count(&source), 1);
    assert_eq!(library.ids().len(), 1);
}
