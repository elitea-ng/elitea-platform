//! `read_document` and `read_file` through a session: the host's extractor
//! is used for documents, and `read_file` on one points at `read_document`.

use std::sync::Arc;

use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
use elitea_local_tools::document::{
    DocumentExtractor, Documents, ExtractFailure, Page, PageRange, PagedText,
};
use elitea_local_tools::policy::LocalWorkPolicy;
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde_json::{Value, json};

struct Fake;

impl DocumentExtractor for Fake {
    fn extract(
        &self,
        mime: &str,
        bytes: Vec<u8>,
        range: &PageRange,
    ) -> Result<PagedText, ExtractFailure> {
        Ok(PagedText {
            extractor: "fake".to_owned(),
            page_count: 2,
            pages: (1..=2)
                .filter(|n| range.contains(*n))
                .map(|number| Page {
                    number,
                    text: format!("{mime} {} bytes, page {number}", bytes.len()),
                })
                .collect(),
        })
    }
}

fn session(documents: Documents) -> (tempfile::TempDir, Arc<LocalSession>) {
    let dir = tempfile::tempdir().expect("dir");
    let root = dir.path().join("work");
    std::fs::create_dir(&root).expect("work");
    std::fs::write(root.join("contract.pdf"), b"%PDF-1.4\n\x00\x01\x02binary").expect("pdf");
    let mut shell = ShellConfig::new(dir.path().join("data/tmp"));
    shell.sandbox.allow_unenforced = true;
    let session = LocalSession::open(SessionConfig {
        root,
        session_id: "s".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            ..LocalWorkPolicy::default()
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::new(DeferringPrompt),
        data_dir: dir.path().join("data"),
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
        budgets: elitea_local_tools::budget::Budgets::default(),
        documents,
    })
    .expect("session");
    (dir, session)
}

async fn call(session: &Arc<LocalSession>, tool: &str, args: Value) -> Value {
    session.call(tool, "c", args).await
}

#[tokio::test]
async fn the_hosts_extractor_reads_documents_with_page_ranges() {
    let (_dir, session) = session(Documents::Custom(Arc::new(Fake)));
    let all = call(&session, "read_document", json!({ "path": "contract.pdf" })).await;
    assert_eq!(all["status"], "ok", "{all}");
    assert_eq!(all["extractor"], "fake");
    assert_eq!(all["page_count"], 2);
    assert_eq!(all["pages"], "1-2");
    let second = call(
        &session,
        "read_document",
        json!({ "path": "contract.pdf", "pages": "2" }),
    )
    .await;
    assert_eq!(second["pages"], "2", "{second}");
    assert!(second["text"].as_str().expect("text").contains("page 2"));
    assert!(!second["text"].as_str().expect("text").contains("page 1"));
}

#[tokio::test]
async fn a_build_without_an_extractor_says_so() {
    let (_dir, session) = session(Documents::InProcess);
    let result = call(&session, "read_document", json!({ "path": "contract.pdf" })).await;
    if cfg!(feature = "documents") {
        return;
    }
    assert_eq!(result["status"], "error", "{result}");
    assert!(
        result["message"]
            .as_str()
            .expect("message")
            .contains("cannot extract text from application/pdf"),
        "{result}"
    );
}

#[tokio::test]
async fn read_file_on_pdf_points_to_read_document() {
    let (_dir, session) = session(Documents::default());
    let result = call(&session, "read_file", json!({ "path": "contract.pdf" })).await;
    assert_eq!(result["status"], "ok", "{result}");
    assert!(
        result["hint"]
            .as_str()
            .expect("hint")
            .contains("read_document"),
        "{result}"
    );
    assert!(result.get("content").is_none(), "{result}");
}
