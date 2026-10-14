//! The six SDK `artifact` tools beyond list/read/create/delete, against an
//! in-memory bucket: what each one reads and writes, and the sentence each
//! failure becomes. The bucket is a `PlatformWriter`, the same seam the
//! cloud adapter and the desktop fill, so the tools run exactly as they do
//! in a turn; only the storage is replaced.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Value, json};

use super::families::artifact::ArtifactToolAuthority;
use super::families::artifact::config::ArtifactToolkitConfig;
use super::families::artifact::tools::build_artifact_toolset;
use super::families::office_host::{
    OfficeBucketCreated, OfficeFailure, OfficeHostError, OfficeModelRequest, OfficeObjectWritten,
    OfficeToolHost,
};
use super::policy::ToolAdmissionPolicy;
use crate::host::{HostError, HostErrorCode, PlatformWriter};
use crate::platform::{
    ArtifactDeleteOutcome, ArtifactDeleteRequest, ArtifactFile, ArtifactListOutcome,
    ArtifactListRequest, ArtifactReadOutcome, ArtifactReadRequest, ArtifactWriteOutcome,
    ArtifactWriteRequest, ProjectContextWriteOutcome, ProjectContextWriteRequest,
    SkillWriteOutcome, SkillWriteRequest,
};

const READ_LIMIT: u64 = 200_000;
const WRITE_LIMIT: usize = 64 * 1_024;

#[derive(Default)]
struct Bucket {
    files: Mutex<BTreeMap<(String, String), String>>,
    /// Every call fails with this until cleared.
    fail_reads: Mutex<Option<HostErrorCode>>,
    fail_writes: Mutex<Option<HostErrorCode>>,
    writes: Mutex<Vec<(String, String, String)>>,
}

impl Bucket {
    fn put(&self, bucket: &str, name: &str, content: &str) {
        self.files
            .lock()
            .expect("files")
            .insert((bucket.to_owned(), name.to_owned()), content.to_owned());
    }

    fn get(&self, bucket: &str, name: &str) -> Option<String> {
        self.files
            .lock()
            .expect("files")
            .get(&(bucket.to_owned(), name.to_owned()))
            .cloned()
    }

    fn writes(&self) -> Vec<(String, String, String)> {
        self.writes.lock().expect("writes").clone()
    }
}

#[async_trait]
impl PlatformWriter for Bucket {
    async fn list_artifacts(
        &self,
        request: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError> {
        if let Some(code) = *self.fail_reads.lock().expect("fail") {
            return Err(HostError::new(code, "fixture"));
        }
        let files = self
            .files
            .lock()
            .expect("files")
            .iter()
            .filter(|((bucket, name), _)| {
                *bucket == request.bucket && name.starts_with(&request.prefix)
            })
            .map(|((_, name), content)| ArtifactFile {
                name: name.clone(),
                byte_length: content.len() as u64,
                media_type: if std::path::Path::new(name)
                    .extension()
                    .is_some_and(|extension| extension.eq_ignore_ascii_case("pdf"))
                {
                    "application/pdf".to_owned()
                } else {
                    "text/plain".to_owned()
                },
                modified_at: "2026-10-01T00:00:00Z".to_owned(),
            })
            .collect();
        Ok(ArtifactListOutcome {
            bucket: request.bucket.clone(),
            files,
            truncated: false,
        })
    }

    async fn read_artifact(
        &self,
        request: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError> {
        if let Some(code) = *self.fail_reads.lock().expect("fail") {
            return Err(HostError::new(code, "fixture"));
        }
        let content = self
            .get(&request.bucket, &request.name)
            .ok_or_else(|| HostError::new(HostErrorCode::NotFound, "missing"))?;
        let chars = content.chars().count() as u64;
        let over_limit = chars > READ_LIMIT;
        Ok(ArtifactReadOutcome {
            name: request.name.clone(),
            media_type: "text/plain".to_owned(),
            byte_length: content.len() as u64,
            char_length: chars,
            total_lines: content.lines().count() as u64,
            max_chars: READ_LIMIT,
            over_limit,
            content: if over_limit { String::new() } else { content },
        })
    }

    async fn write_artifact(
        &self,
        request: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError> {
        if let Some(code) = *self.fail_writes.lock().expect("fail") {
            return Err(HostError::new(code, "fixture"));
        }
        if request.content.chars().count() > WRITE_LIMIT {
            return Err(HostError::new(HostErrorCode::ResourceExhausted, "too big"));
        }
        self.writes.lock().expect("writes").push((
            request.bucket.clone(),
            request.name.clone(),
            request.content.clone(),
        ));
        self.put(&request.bucket, &request.name, &request.content);
        Ok(ArtifactWriteOutcome {
            bucket: request.bucket.clone(),
            name: request.name.clone(),
            media_type: "text/plain".to_owned(),
            byte_length: request.content.len() as u64,
        })
    }

    async fn delete_artifact(
        &self,
        request: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError> {
        Ok(ArtifactDeleteOutcome {
            name: request.name.clone(),
            deleted: true,
        })
    }

    async fn write_skill(
        &self,
        _request: &SkillWriteRequest,
    ) -> Result<SkillWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::InvalidInput, "unused"))
    }

    async fn write_project_context(
        &self,
        _request: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::InvalidInput, "unused"))
    }
}

#[derive(Default)]
struct Buckets {
    created: Mutex<Vec<(String, Option<u32>)>>,
    fail: Mutex<Option<OfficeFailure>>,
}

#[async_trait]
impl OfficeToolHost for Buckets {
    async fn read_object(&self, _: &str, _: &str, _: u64) -> Result<Vec<u8>, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::NotFound, "unused"))
    }

    async fn write_object(
        &self,
        _: &str,
        _: &str,
        _: &str,
        _: Vec<u8>,
        _: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::Unsupported, "unused"))
    }

    async fn create_bucket(
        &self,
        name: &str,
        retention_days: Option<u32>,
    ) -> Result<OfficeBucketCreated, OfficeHostError> {
        if let Some(kind) = *self.fail.lock().expect("fail") {
            return Err(OfficeHostError::new(kind, "fixture"));
        }
        self.created
            .lock()
            .expect("created")
            .push((name.to_owned(), retention_days));
        Ok(OfficeBucketCreated {
            name: name.to_owned(),
            retention_days,
        })
    }

    async fn complete(&self, _: OfficeModelRequest) -> Result<String, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::NoModel, "unused"))
    }
}

struct Rig {
    tools: Vec<Arc<dyn Tool>>,
    bucket: Arc<Bucket>,
}

async fn rig_with(office: Option<Arc<Buckets>>) -> Rig {
    let bucket = Arc::new(Bucket::default());
    let mut authority = ArtifactToolAuthority::new(bucket.clone());
    if let Some(office) = office {
        authority = authority.with_office(office as Arc<dyn OfficeToolHost>);
    }
    let settings = json!({"bucket": "docs", "selected_tools": []});
    let config =
        ArtifactToolkitConfig::parse(settings.as_object().expect("settings")).expect("config");
    let policy = Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"));
    let toolset = build_artifact_toolset("files", &config, &policy, &authority).expect("toolset");
    let context: Arc<dyn ReadonlyContext> = Arc::new(SimpleToolContext::new("artifact-extra"));
    let tools = toolset.tools(context).await.expect("tools");
    Rig { tools, bucket }
}

async fn rig() -> Rig {
    rig_with(None).await
}

impl Rig {
    async fn says(&self, name: &str, arguments: Value) -> String {
        match self.call(name, arguments).await {
            Value::String(text) => text,
            other => panic!("{name} answered {other}, not text"),
        }
    }

    async fn call(&self, name: &str, arguments: Value) -> Value {
        let tool = self
            .tools
            .iter()
            .find(|tool| tool.name() == name)
            .unwrap_or_else(|| panic!("no tool {name}"));
        let context: Arc<dyn ToolContext> = Arc::new(
            SimpleToolContext::new("artifact-extra")
                .with_session_id("session")
                .with_function_call_id("call"),
        );
        tool.execute(context, arguments)
            .await
            .unwrap_or_else(|error| panic!("{name} returned an error: {error}"))
    }
}

// ----- the set ---------------------------------------------------------------------

#[tokio::test]
async fn the_family_serves_ten_of_the_sixteen_sdk_tools_and_keeps_their_contract() {
    let rig = rig().await;
    let mut names: Vec<_> = rig
        .tools
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect();
    names.sort();
    assert_eq!(
        names,
        [
            "append_data",
            "create_file",
            "create_new_bucket",
            "delete_file",
            "edit_file",
            "get_file_metadata",
            "grep_file",
            "list_files",
            "read_file",
            "read_multiple_files",
        ]
    );
    super::sdk_conformance::assert_sdk_conformance("artifact", &rig.tools);
    for tool in &rig.tools {
        let reads = matches!(
            tool.name(),
            "list_files" | "read_file" | "read_multiple_files" | "grep_file" | "get_file_metadata"
        );
        assert_eq!(tool.is_read_only(), reads, "{}", tool.name());
        assert_eq!(tool.is_concurrency_safe(), reads, "{}", tool.name());
    }
}

// ----- read_multiple_files --------------------------------------------------------

#[tokio::test]
async fn read_multiple_files_reads_in_order_with_the_sdk_path_forms() {
    let rig = rig().await;
    rig.bucket.put("docs", "a.txt", "l1\nl2\nl3\n");
    rig.bucket.put("docs", "dir/b.txt", "bee");
    rig.bucket.put("other", "c.txt", "see");
    let result = rig
        .call(
            "read_multiple_files",
            json!({"file_paths": ["a.txt", "/other/c.txt", "dir/b.txt", "nope.txt"]}),
        )
        .await;
    assert_eq!(result["a.txt"], "l1\nl2\nl3\n");
    assert_eq!(
        result["/other/c.txt"], "see",
        "a leading / names the bucket"
    );
    assert_eq!(result["dir/b.txt"], "bee");
    assert_eq!(
        result["nope.txt"],
        "Error reading file: no such file or bucket. Check the name with list_files."
    );
    // The names round-trip as the keys they were asked for.
    let keys: Vec<_> = result.as_object().expect("map").keys().cloned().collect();
    assert_eq!(keys.len(), 4);

    // offset/limit are lines, 1-indexed, for every file.
    let result = rig
        .call(
            "read_multiple_files",
            json!({"file_paths": ["a.txt"], "offset": 2, "limit": 1}),
        )
        .await;
    assert_eq!(result["a.txt"], "l2\n");
    let result = rig
        .call(
            "read_multiple_files",
            json!({"file_paths": ["a.txt"], "offset": 9}),
        )
        .await;
    assert_eq!(
        result["a.txt"],
        "Error reading file: the line range is empty; the file has 3 lines."
    );
}

#[tokio::test]
async fn read_multiple_files_stops_reading_at_the_cumulative_cap() {
    let rig = rig().await;
    let big = "x".repeat(120_000);
    for name in ["one", "two", "three"] {
        rig.bucket.put("docs", name, &big);
    }
    let result = rig
        .call(
            "read_multiple_files",
            json!({"file_paths": ["one", "two", "three"]}),
        )
        .await;
    assert_eq!(result["one"].as_str().expect("text").len(), 120_000);
    assert_eq!(result["two"].as_str().expect("text").len(), 120_000);
    let skipped = result["three"].as_str().expect("notice");
    assert!(
        skipped.starts_with("Skipped: the batch's cumulative 200000-character"),
        "{skipped}"
    );

    // A file over the per-file cap is the structured guidance, and it counts.
    rig.bucket.put("docs", "huge", &"y".repeat(250_000));
    let result = rig
        .call(
            "read_multiple_files",
            json!({"file_paths": ["huge", "one"]}),
        )
        .await;
    assert_eq!(result["huge"]["__result_status__"], "content_too_large");
    assert_eq!(result["one"].as_str().expect("text").len(), 120_000);
}

#[tokio::test]
async fn read_multiple_files_bounds_the_batch() {
    let rig = rig().await;
    let paths: Vec<String> = (0..33).map(|index| format!("f{index}")).collect();
    let says = rig
        .says("read_multiple_files", json!({"file_paths": paths}))
        .await;
    assert!(says.contains("at most 32 files"), "{says}");
    let says = rig
        .says("read_multiple_files", json!({"file_paths": []}))
        .await;
    assert!(says.contains("at least one"), "{says}");
    let says = rig
        .says(
            "read_multiple_files",
            json!({"file_paths": ["a"], "offset": 0}),
        )
        .await;
    assert!(says.contains("must be at least 1"), "{says}");
}

// ----- grep_file -----------------------------------------------------------------------

#[tokio::test]
async fn grep_file_matches_the_sdk_layout() {
    let rig = rig().await;
    rig.bucket
        .put("docs", "log.txt", "alpha\nbeta\ngamma\nBETA two\n");
    let found = rig
        .says(
            "grep_file",
            json!({"file_path": "log.txt", "pattern": "beta", "context_lines": 1}),
        )
        .await;
    assert_eq!(
        found,
        "Found 2 match(es) in log.txt:\n\n\n--- Line 2 ---\n  alpha\n> beta\n  gamma\n\n--- Line 4 ---\n  gamma\n> BETA two"
    );
    // Literal search treats the pattern as text; a bad regex finds nothing.
    rig.bucket.put("docs", "code.txt", "f(x)\nplain\n");
    assert_eq!(
        rig.says("grep_file", json!({"file_path": "code.txt", "pattern": "f(x)", "is_regex": false, "context_lines": 0})).await,
        "Found 1 match(es) in code.txt:\n\n\n--- Line 1 ---\n> f(x)"
    );
    assert_eq!(
        rig.says(
            "grep_file",
            json!({"file_path": "code.txt", "pattern": "f("})
        )
        .await,
        "No matches found for pattern 'f(' in code.txt"
    );
    assert_eq!(
        rig.says(
            "grep_file",
            json!({"file_path": "code.txt", "pattern": "zzz"})
        )
        .await,
        "No matches found for pattern 'zzz' in code.txt"
    );
}

#[tokio::test]
async fn grep_file_failures_and_bounds() {
    let rig = rig().await;
    assert_eq!(
        rig.says(
            "grep_file",
            json!({"file_path": "gone.txt", "pattern": "x"})
        )
        .await,
        "Could not search the file: no such file or bucket. Check the name with list_files."
    );
    rig.bucket.put("docs", "huge.txt", &"x\n".repeat(150_000));
    let says = rig
        .says(
            "grep_file",
            json!({"file_path": "huge.txt", "pattern": "x"}),
        )
        .await;
    assert!(
        says.contains("over the 200000 characters this runtime reads at once"),
        "{says}"
    );
    let says = rig
        .says(
            "grep_file",
            json!({"file_path": "huge.txt", "pattern": "x", "context_lines": 99}),
        )
        .await;
    assert!(
        says.contains("context_lines is a count from 0 to 32"),
        "{says}"
    );

    // 3000 matching lines: the output stops at the bound and says so.
    rig.bucket.put("docs", "many.txt", &"hit\n".repeat(3_000));
    let says = rig
        .says(
            "grep_file",
            json!({"file_path": "many.txt", "pattern": "hit", "context_lines": 0}),
        )
        .await;
    assert!(
        says.starts_with("Found 3000 match(es) in many.txt:"),
        "{says}"
    );
    assert!(says.contains("1000 more match(es) are not shown"), "{says}");
    assert!(says.chars().count() <= 200_300, "bounded");

    *rig.bucket.fail_reads.lock().expect("fail") = Some(HostErrorCode::AuthorizationFailed);
    let says = rig
        .says(
            "grep_file",
            json!({"file_path": "many.txt", "pattern": "hit"}),
        )
        .await;
    assert!(says.contains("not allowed to use that bucket"), "{says}");
}

// ----- edit_file -------------------------------------------------------------------------

fn edit(old: &str, new: &str) -> String {
    format!("OLD <<<<\n{old}\n>>>> OLD\nNEW <<<<\n{new}\n>>>> NEW\n")
}

#[tokio::test]
async fn edit_file_applies_the_pairs_that_match_and_reports_the_rest() {
    let rig = rig().await;
    rig.bucket
        .put("docs", "notes.md", "# Title\nhello world\nfoo\nfoo\n");
    let query = format!(
        "{}{}{}",
        edit("hello world", "hello mars"),
        edit("foo", "bar"),
        edit("missing", "x")
    );
    let says = rig
        .says(
            "edit_file",
            json!({"file_path": "notes.md", "file_query": query}),
        )
        .await;
    assert!(
        says.starts_with(
            "Successfully applied 1 edit(s) to notes.md. WARNING: 2 edit(s) were NOT applied: "
        ),
        "{says}"
    );
    assert!(says.contains("Exact OLD block appears 2 times in notes.md; no replacement applied to avoid ambiguity. OLD value: 'foo'"), "{says}");
    assert!(
        says.contains("Normalized OLD block not found in notes.md. OLD value: 'missing'"),
        "{says}"
    );
    assert_eq!(
        rig.bucket.get("docs", "notes.md").as_deref(),
        Some("# Title\nhello mars\nfoo\nfoo\n")
    );
    assert_eq!(rig.bucket.writes().len(), 1);
}

#[tokio::test]
async fn edit_file_writes_nothing_when_nothing_applies() {
    let rig = rig().await;
    rig.bucket.put("docs", "notes.md", "hello\n");
    let says = rig
        .says(
            "edit_file",
            json!({"file_path": "notes.md", "file_query": edit("absent", "x")}),
        )
        .await;
    assert!(
        says.starts_with("No edits were applied to notes.md. Normalized OLD block not found"),
        "{says}"
    );
    assert_eq!(
        rig.says(
            "edit_file",
            json!({"file_path": "notes.md", "file_query": "no markers"})
        )
        .await,
        "Could not edit the file: No valid OLD/NEW marker pairs found in edit instructions. Markers must be on their own dedicated line."
    );
    let says = rig
        .says(
            "edit_file",
            json!({"file_path": "image.png", "file_query": edit("a", "b")}),
        )
        .await;
    assert!(says.contains("is not a text-editable file type"), "{says}");
    assert_eq!(
        rig.says(
            "edit_file",
            json!({"file_path": "gone.md", "file_query": edit("a", "b")})
        )
        .await,
        "Could not edit the file: no such file or bucket. Check the name with list_files; create_file makes a new file."
    );
    assert!(rig.bucket.writes().is_empty());
}

#[tokio::test]
async fn edit_file_refuses_what_it_cannot_write_back_whole() {
    let rig = rig().await;
    // A big file whose edit would push it over the write cap is refused
    // before any write, with the real size.
    let near = "a".repeat(WRITE_LIMIT - 10);
    rig.bucket
        .put("docs", "big.txt", &format!("{near}\nmarker\n"));
    let says = rig
        .says(
            "edit_file",
            json!({"file_path": "big.txt", "file_query": edit("marker", &"m".repeat(200))}),
        )
        .await;
    assert!(
        says.contains("over the 65536 this platform accepts in one write"),
        "{says}"
    );
    assert!(says.contains("Nothing was changed"), "{says}");

    // A file over the read cap is not edited from a partial view.
    rig.bucket.put("docs", "huge.txt", &"z".repeat(250_000));
    let says = rig
        .says(
            "edit_file",
            json!({"file_path": "huge.txt", "file_query": edit("z", "y")}),
        )
        .await;
    assert!(
        says.contains("over the 200000 characters this runtime reads at once"),
        "{says}"
    );
    assert!(rig.bucket.writes().is_empty());
}

#[tokio::test]
async fn edit_file_failures_are_told_apart() {
    for (code, expected) in [
        (
            HostErrorCode::AuthorizationFailed,
            "not allowed to use that bucket",
        ),
        (HostErrorCode::DependencyUnavailable, "it did not happen"),
    ] {
        let rig = rig().await;
        rig.bucket.put("docs", "n.md", "hello\n");
        *rig.bucket.fail_reads.lock().expect("fail") = Some(code);
        let says = rig
            .says(
                "edit_file",
                json!({"file_path": "n.md", "file_query": edit("hello", "bye")}),
            )
            .await;
        assert!(says.contains(expected), "{says}");
        assert!(rig.bucket.writes().is_empty());
    }
    for (code, expected) in [
        (
            HostErrorCode::AuthorizationFailed,
            "not allowed to use that bucket",
        ),
        (
            HostErrorCode::ResourceExhausted,
            "larger than this platform accepts",
        ),
        (
            HostErrorCode::DependencyUnavailable,
            "it is not known whether the change was made",
        ),
    ] {
        let rig = rig().await;
        rig.bucket.put("docs", "n.md", "hello\n");
        *rig.bucket.fail_writes.lock().expect("fail") = Some(code);
        let says = rig
            .says(
                "edit_file",
                json!({"file_path": "n.md", "file_query": edit("hello", "bye")}),
            )
            .await;
        assert!(
            says.starts_with("Could not save the edited file:"),
            "{says}"
        );
        assert!(says.contains(expected), "{says}");
    }
}

// ----- append_data -------------------------------------------------------------------------

#[tokio::test]
async fn append_data_adds_after_a_newline_and_creates_a_missing_file() {
    let rig = rig().await;
    rig.bucket.put("docs", "log.txt", "first");
    let says = rig
        .says(
            "append_data",
            json!({"filename": "log.txt", "filedata": "second"}),
        )
        .await;
    assert_eq!(
        says,
        r#"{"filepath": "/docs/log.txt", "filename": "log.txt", "bucket": "docs", "message": "Data appended successfully"}"#
    );
    assert_eq!(
        rig.bucket.get("docs", "log.txt").as_deref(),
        Some("first\nsecond")
    );

    // An empty file takes the data without a leading newline.
    rig.bucket.put("docs", "empty.txt", "");
    rig.says(
        "append_data",
        json!({"filename": "empty.txt", "filedata": "x"}),
    )
    .await;
    assert_eq!(rig.bucket.get("docs", "empty.txt").as_deref(), Some("x"));

    // A missing file is created from the data (create_if_missing defaults on).
    rig.says(
        "append_data",
        json!({"filename": "/new/part.txt", "filedata": "hello"}),
    )
    .await;
    assert_eq!(
        rig.bucket.get("docs", "new/part.txt").as_deref(),
        Some("hello")
    );

    // ...unless the caller said not to.
    let says = rig
        .says(
            "append_data",
            json!({"filename": "nothing.txt", "filedata": "x", "create_if_missing": false}),
        )
        .await;
    assert!(
        says.starts_with("Failed to append to file 'nothing.txt'"),
        "{says}"
    );
    assert!(rig.bucket.get("docs", "nothing.txt").is_none());
}

#[tokio::test]
async fn append_data_never_overwrites_a_file_it_could_not_read() {
    for code in [
        HostErrorCode::AuthorizationFailed,
        HostErrorCode::DependencyUnavailable,
        HostErrorCode::InvalidInput,
    ] {
        let rig = rig().await;
        rig.bucket.put("docs", "precious.txt", "keep me");
        *rig.bucket.fail_reads.lock().expect("fail") = Some(code);
        let says = rig
            .says(
                "append_data",
                json!({"filename": "precious.txt", "filedata": "more"}),
            )
            .await;
        assert!(says.starts_with("Could not append to the file:"), "{says}");
        assert!(
            rig.bucket.writes().is_empty(),
            "{code:?}: a failed read must not create or replace"
        );
        assert_eq!(
            rig.bucket.get("docs", "precious.txt").as_deref(),
            Some("keep me")
        );
    }
}

#[tokio::test]
async fn append_data_refuses_what_it_cannot_do_whole() {
    let rig = rig().await;
    rig.bucket
        .put("docs", "full.txt", &"a".repeat(WRITE_LIMIT - 5));
    let says = rig
        .says(
            "append_data",
            json!({"filename": "full.txt", "filedata": "0123456789"}),
        )
        .await;
    assert!(
        says.contains("over the 65536 this platform accepts in one write"),
        "{says}"
    );
    assert!(says.contains("Nothing was changed"), "{says}");
    for name in ["report.docx", "scan.pdf", "sheet.xlsx"] {
        let says = rig
            .says("append_data", json!({"filename": name, "filedata": "x"}))
            .await;
        assert!(
            says.starts_with(&format!("Cannot append to '{name}'")),
            "{says}"
        );
    }
    rig.bucket.put("docs", "data.bin", "bytes");
    let says = rig
        .says(
            "append_data",
            json!({"filename": "data.bin", "filedata": "x"}),
        )
        .await;
    assert!(says.contains("File type is not text-editable"), "{says}");
    assert!(rig.bucket.writes().is_empty());
}

#[tokio::test]
async fn append_data_write_failures_say_the_outcome_may_be_unknown() {
    let rig = rig().await;
    rig.bucket.put("docs", "log.txt", "first");
    *rig.bucket.fail_writes.lock().expect("fail") = Some(HostErrorCode::DependencyUnavailable);
    let says = rig
        .says(
            "append_data",
            json!({"filename": "log.txt", "filedata": "x"}),
        )
        .await;
    assert!(
        says.starts_with("Failed to append to file 'log.txt'"),
        "{says}"
    );
    assert!(
        says.contains("it is not known whether the change was made"),
        "{says}"
    );
}

// ----- get_file_metadata ---------------------------------------------------------------------

#[tokio::test]
async fn get_file_metadata_reports_type_size_and_lines() {
    let rig = rig().await;
    rig.bucket.put("docs", "notes.md", "a\nb\nc");
    let json_text = rig
        .says("get_file_metadata", json!({"filename": "notes.md"}))
        .await;
    let meta: Value = serde_json::from_str(&json_text).expect("JSON text");
    assert_eq!(meta["__result_status__"], "file_metadata");
    assert_eq!(meta["schema_version"], "1.0");
    assert_eq!(meta["filename"], "notes.md");
    assert_eq!(meta["extension"], ".md");
    assert_eq!(meta["filesize"], 5);
    assert_eq!(meta["bucket"], "docs");
    assert_eq!(meta["unit"], "lines");
    assert_eq!(meta["total_lines"], 3);
    assert_eq!(
        meta["read_limits"],
        json!({"max_output_chars": 200_000, "full_read_allowed": true})
    );
    assert!(
        meta["instruction_for_readFile"]["first_class_params"]["start_line"]
            .as_str()
            .expect("hint")
            .contains("Valid range 1..3")
    );

    // The filepath form names the bucket, and comes back.
    rig.bucket.put("other", "x/y.txt", "q");
    let json_text = rig
        .says("get_file_metadata", json!({"filepath": "/other/x/y.txt"}))
        .await;
    let meta: Value = serde_json::from_str(&json_text).expect("JSON text");
    assert_eq!(meta["bucket"], "other");
    assert_eq!(meta["filepath"], "/other/x/y.txt");
    assert_eq!(meta["filename"], "x/y.txt");

    // A binary file: size and type from the listing, no structure, said so.
    rig.bucket.put("docs", "scan.pdf", "%PDF-1.7 ...");
    let json_text = rig
        .says("get_file_metadata", json!({"filename": "scan.pdf"}))
        .await;
    let meta: Value = serde_json::from_str(&json_text).expect("JSON text");
    assert_eq!(meta["type"], "application/pdf");
    assert_eq!(meta["unit"], Value::Null);
    assert!(
        meta["instruction_for_readFile"]["notes"]
            .as_str()
            .expect("notes")
            .contains("not available in this runtime")
    );
}

#[tokio::test]
async fn get_file_metadata_errors_are_the_sdk_error_object() {
    let rig = rig().await;
    for (arguments, expected) in [
        (json!({}), "Must provide either 'filepath' or 'filename'."),
        (
            json!({"filepath": "no-slash"}),
            "Invalid filepath format: filepath must be in /{bucket}/{filename} form.",
        ),
        (
            json!({"filename": "gone.txt"}),
            "File 'gone.txt' was not found in bucket 'docs'.",
        ),
    ] {
        let text = rig.says("get_file_metadata", arguments).await;
        let meta: Value = serde_json::from_str(&text).expect("JSON text");
        assert_eq!(meta["__result_status__"], "error", "{text}");
        assert_eq!(meta["message"], expected, "{text}");
    }
    *rig.bucket.fail_reads.lock().expect("fail") = Some(HostErrorCode::AuthorizationFailed);
    let text = rig
        .says("get_file_metadata", json!({"filename": "gone.txt"}))
        .await;
    let meta: Value = serde_json::from_str(&text).expect("JSON text");
    assert!(
        meta["message"]
            .as_str()
            .expect("m")
            .contains("not allowed to use that bucket"),
        "{text}"
    );
}

// ----- create_new_bucket -----------------------------------------------------------------------

#[tokio::test]
async fn create_new_bucket_sanitizes_the_name_and_converts_the_expiry_to_days() {
    let office = Arc::new(Buckets::default());
    let rig = rig_with(Some(office.clone())).await;
    let result = rig
        .call(
            "create_new_bucket",
            json!({"bucket_name": "My_Reports", "expiration_measure": "weeks", "expiration_value": 2}),
        )
        .await;
    assert_eq!(result["name"], "my-reports");
    assert_eq!(result["retention_days"], 14);
    assert!(
        result["message"]
            .as_str()
            .expect("m")
            .contains("the name 'My_Reports' was changed to 'my-reports'"),
        "{result}"
    );
    rig.call(
        "create_new_bucket",
        json!({"bucket_name": "temp", "expiration_measure": "months", "expiration_value": 3}),
    )
    .await;
    rig.call("create_new_bucket", json!({"bucket_name": "year"}))
        .await;
    rig.call(
        "create_new_bucket",
        json!({"bucket_name": "yrs", "expiration_measure": "years"}),
    )
    .await;
    let created = office.created.lock().expect("created").clone();
    assert_eq!(
        created,
        [
            ("my-reports".to_owned(), Some(14)),
            ("temp".to_owned(), Some(90)),
            ("year".to_owned(), Some(7)),
            ("yrs".to_owned(), Some(365)),
        ]
    );
}

#[tokio::test]
async fn create_new_bucket_refuses_bad_input_before_asking_the_platform() {
    let office = Arc::new(Buckets::default());
    let rig = rig_with(Some(office.clone())).await;
    for (arguments, expected) in [
        (json!({"bucket_name": "a"}), "not a valid bucket name"),
        (
            json!({"bucket_name": "ok-name", "expiration_measure": "fortnights"}),
            "is not one of days, weeks, months, years",
        ),
        (
            json!({"bucket_name": "ok-name", "expiration_value": 0}),
            "at least 1",
        ),
        (
            json!({"bucket_name": "ok-name", "expiration_measure": "years", "expiration_value": 99_999_999}),
            "too long",
        ),
    ] {
        let says = rig.says("create_new_bucket", arguments).await;
        assert!(says.starts_with("Could not create the bucket:"), "{says}");
        assert!(says.contains(expected), "{expected}: {says}");
        assert!(says.contains("Nothing was created"), "{says}");
    }
    assert!(office.created.lock().expect("created").is_empty());
}

#[tokio::test]
async fn create_new_bucket_failures_are_told_apart() {
    for (kind, expected) in [
        (
            OfficeFailure::AlreadyExists,
            "a bucket named 'taken' already exists",
        ),
        (OfficeFailure::AccessRefused, "may not create buckets"),
        (OfficeFailure::InvalidRequest, "refused 'taken' as invalid"),
        (OfficeFailure::Unsupported, "does not offer bucket creation"),
        (
            OfficeFailure::Unreachable,
            "not known whether 'taken' was created",
        ),
    ] {
        let office = Arc::new(Buckets::default());
        *office.fail.lock().expect("fail") = Some(kind);
        let rig = rig_with(Some(office)).await;
        let says = rig
            .says("create_new_bucket", json!({"bucket_name": "taken"}))
            .await;
        assert!(says.contains(expected), "{kind:?}: {says}");
    }
    // A host with no platform access says so.
    let rig = rig().await;
    let says = rig
        .says("create_new_bucket", json!({"bucket_name": "fresh"}))
        .await;
    assert!(
        says.contains("no access to the platform's bucket API"),
        "{says}"
    );
}
