//! SDK-parity fixes found by the indexing audit: what a run keeps of a
//! listing (the code families' extension allowlist, the artifact folder and
//! patterns), the attested-empty guard (EL-6232), the status of a run that
//! skipped documents, the `chunking_tool` forms of the `ado_plans` toolkit,
//! the base-document-only `chunking_tool`, the code metadata keys, and the
//! ignored loader settings.

use elitea_vector_index::format::code_page_content;
use elitea_vector_index::hit::Hit;
use serde_json::{Map, Value, json};

use super::inputs::{IndexParameters, ignored_loader_keys};
use super::pipeline::skip_loses_document;
use super::source::{PathFilter, with_code_metadata};
use super::tests::{Always, NS, command, document, harness, parameters, run, stored};
use crate::protocol::elitea::runtime::v1::IndexIngestStatusV1;

fn parameters_of(arguments: &Value) -> IndexParameters {
    let map = arguments.as_object().cloned().unwrap_or_default();
    let list = |key: &str| -> Vec<String> {
        map.get(key)
            .and_then(Value::as_array)
            .map(|items| {
                items
                    .iter()
                    .filter_map(|item| item.as_str().map(str::to_owned))
                    .collect()
            })
            .unwrap_or_default()
    };
    IndexParameters {
        index_name: "x".to_owned(),
        clean_index: false,
        chunking_tool: None,
        chunking_config: Value::Null,
        branch: None,
        whitelist: list("whitelist"),
        blacklist: list("blacklist"),
        arguments: map,
    }
}

#[allow(clippy::needless_pass_by_value)] // Call sites pass `json!` literals.
fn filter(toolkit: &str, arguments: Value) -> PathFilter {
    PathFilter::for_run(toolkit, &parameters_of(&arguments)).expect("a valid filter")
}

// ---- gap 6: skip_unsupported_extensions ---------------------------------

#[test]
fn the_git_families_index_only_the_sdks_supported_extensions_by_default() {
    for family in ["github", "gitlab", "bitbucket", "ado_repos"] {
        let kept = filter(family, json!({}));
        for path in [
            "README.md",
            "src/app.py",
            "docs/Guide.MDX",
            "config/app.YAML",
            "deploy/prod.env",
            "data/rows.csv",
            "web/index.htm",
            "scripts/run.zsh",
            "lib/Main.hs",
            "a/b.c/d.rs",
        ] {
            assert!(kept.keeps(path), "{family}: {path} is supported");
        }
        for path in [
            "logo.png",
            "manual.pdf",
            "archive.tar.gz",
            "Makefile",
            "Dockerfile",
            ".bashrc",
            ".env",
            "notes",
            "build/app.exe",
            "dir.d/noext",
        ] {
            assert!(!kept.keeps(path), "{family}: {path} is not supported");
        }
    }
}

#[test]
fn false_indexes_every_file_and_other_families_are_unfiltered() {
    let all = filter("github", json!({"skip_unsupported_extensions": false}));
    for path in ["logo.png", "manual.pdf", "Makefile", ".bashrc", "a.py"] {
        assert!(all.keeps(path), "{path}");
    }
    // An explicit true is the default.
    assert!(!filter("gitlab", json!({"skip_unsupported_extensions": true})).keeps("logo.png"));
    // The argument means nothing to a wiki or a bucket.
    for family in ["confluence", "jira", "sharepoint", "fixture"] {
        assert!(filter(family, json!({})).keeps("logo.png"), "{family}");
    }
}

#[test]
fn the_extension_allowlist_composes_with_the_whitelist_and_blacklist() {
    let both = filter(
        "github",
        json!({"whitelist": ["*.py", "*.png"], "blacklist": ["*test_*"]}),
    );
    assert!(both.keeps("src/app.py"));
    assert!(!both.keeps("tests/test_app.py"));
    // Whitelisted but not a supported type.
    assert!(!both.keeps("logo.png"));
}

#[test]
fn a_mistyped_skip_unsupported_extensions_is_refused_with_its_name() {
    let error = PathFilter::for_run(
        "github",
        &parameters_of(&json!({"skip_unsupported_extensions": "no"})),
    )
    .expect_err("refused");
    assert!(error.contains("skip_unsupported_extensions"), "{error}");
}

// ---- gap 3: the artifact folder and patterns ----------------------------

#[test]
fn artifact_patterns_are_the_sdks_case_insensitive_fnmatch_over_the_full_key() {
    let docs = filter(
        "artifact",
        json!({"folder": "docs", "include_extensions": ["docs/*.MD", "*.pdf"], "skip_extensions": ["docs/draft/*", "*.TMP"]}),
    );
    // Keys are relative to the folder; the patterns meet `docs/<key>`.
    assert!(docs.keeps("guide.md"));
    assert!(docs.keeps("GUIDE.Md"));
    assert!(docs.keeps("deep/er/report.pdf"));
    assert!(!docs.keeps("draft/idea.md"), "skip wins");
    assert!(!docs.keeps("guide.txt"), "not included");
    assert!(!docs.keeps("scratch.tmp"));
    // A bare extension is not `*.ext` here, as in the SDK.
    let bare = filter("artifact", json!({"include_extensions": ["pdf"]}));
    assert!(!bare.keeps("a.pdf"));
    assert!(filter("artifact", json!({"include_extensions": ["*.pdf"]})).keeps("a.pdf"));
    // Nothing given: everything.
    let open = filter("artifact", json!({}));
    assert!(open.keeps("anything.bin"));
    // Skip alone leaves the rest in.
    let skip_only = filter("artifact", json!({"skip_extensions": ["tmp/*"]}));
    assert!(skip_only.keeps("a.bin"));
    assert!(!skip_only.keeps("tmp/a.bin"));
}

#[test]
fn only_the_whole_bucket_attests_an_empty_listing() {
    assert!(filter("artifact", json!({})).attests_empty());
    assert!(filter("artifact", json!({"folder": "/"})).attests_empty());
    assert!(!filter("artifact", json!({"folder": "docs"})).attests_empty());
    for family in ["github", "jira", "confluence", "sharepoint", "ado_wiki"] {
        assert!(!filter(family, json!({})).attests_empty(), "{family}");
    }
}

#[test]
fn mistyped_artifact_arguments_are_refused_with_their_names() {
    for (arguments, name) in [
        (json!({"folder": 3}), "folder"),
        (json!({"include_extensions": "*.md"}), "include_extensions"),
        (json!({"skip_extensions": [1]}), "skip_extensions"),
    ] {
        let error =
            PathFilter::for_run("artifact", &parameters_of(&arguments)).expect_err("refused");
        assert!(error.contains(name), "{error}");
    }
}

// ---- gap 2: code metadata ------------------------------------------------

#[test]
fn a_repository_file_carries_the_sdks_code_keys_and_prints_its_path() {
    let mut fetched = document("src/app.py", "blobsha", "def main():\n    return 1\n");
    fetched
        .metadata
        .insert("sha".to_owned(), Value::String("blobsha".to_owned()));
    let fetched = with_code_metadata(fetched);
    assert_eq!(fetched.metadata["filename"], "src/app.py");
    assert_eq!(fetched.metadata["file_path"], "src/app.py");
    // The SDK's `commit_hash` is the SHA-256 of the text.
    assert_eq!(
        fetched.metadata["commit_hash"],
        sha256_hex("def main():\n    return 1\n")
    );
    assert_eq!(fetched.metadata["commit_hash"].as_str().unwrap().len(), 64);
    assert_eq!(
        fetched.metadata["sha"], "blobsha",
        "the connector's keys stay"
    );
    // A key the connector already set is not replaced.
    let mut preset = document("a.py", "v", "x");
    preset
        .metadata
        .insert("filename".to_owned(), Value::String("kept".to_owned()));
    assert_eq!(with_code_metadata(preset).metadata["filename"], "kept");
    // What the search prints for a code chunk of that file.
    let mut metadata: Map<String, Value> = fetched.metadata.clone();
    metadata.insert("method_name".to_owned(), json!("main"));
    let hit = Hit {
        key: "src/app.py_1".to_owned(),
        page_content: "def main():\n    return 1".to_owned(),
        metadata,
        score: 0.9,
        dense_score: 0.9,
        document_key: "src/app.py".to_owned(),
        chunk_id: "1".to_owned(),
    };
    let printed = code_page_content(&hit);
    assert!(
        printed.starts_with("src/app.py -> main (score: 0.9)"),
        "{printed}"
    );
    assert!(!printed.contains("unknown"), "{printed}");
}

fn sha256_hex(text: &str) -> String {
    use sha2::{Digest as _, Sha256};
    format!("{:x}", Sha256::digest(text.as_bytes()))
}

// ---- gap 7: the attested-empty guard --------------------------------------

fn two_documents() -> Vec<elitea_content_source::Document> {
    vec![
        document("a.md", "v1", "# A\n\nalpha text\n"),
        document("b.py", "v1", "def b():\n    return 2\n"),
    ]
}

#[tokio::test(flavor = "multi_thread")]
async fn a_whitelist_typo_leaves_a_populated_index_intact_and_fails_the_run() {
    let harness = harness(4, None).await;
    let first = run(
        &harness,
        two_documents(),
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    assert_eq!(first.indexed_documents, 2, "{first:?}");
    let points_before = harness.fake.point_count();

    let typo = run(
        &harness,
        two_documents(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({"whitelist": ["*.mdd"]})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(typo.status, IndexIngestStatusV1::Error as i32, "{typo:?}");
    assert!(typo.message.contains("left untouched"), "{}", typo.message);
    assert!(typo.message.contains("filters"), "{}", typo.message);
    assert!(typo.removed_document_keys.is_empty());
    assert_eq!(harness.fake.point_count(), points_before);
    assert_eq!(stored(&harness).await.len(), 2);

    // A clean run is no way round it: the wipe would lose the index to the
    // same typo.
    let clean = run(
        &harness,
        two_documents(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(true, json!({"whitelist": ["*.mdd"]})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(clean.status, IndexIngestStatusV1::Error as i32, "{clean:?}");
    assert_eq!(harness.fake.point_count(), points_before);

    // The blacklist that excludes everything is the same.
    let blacklisted = run(
        &harness,
        two_documents(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({"blacklist": ["md", "py"]})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(blacklisted.status, IndexIngestStatusV1::Error as i32);
    assert_eq!(harness.fake.point_count(), points_before);

    // The corrected whitelist still removes what it no longer lists, as
    // before: a partial listing is not the guard's business.
    let narrowed = run(
        &harness,
        two_documents(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({"whitelist": ["*.md"]})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        narrowed.status,
        IndexIngestStatusV1::Ok as i32,
        "{narrowed:?}"
    );
    assert_eq!(narrowed.removed_document_keys, vec!["b.py"]);
}

#[tokio::test(flavor = "multi_thread")]
async fn an_empty_listing_deletes_only_when_the_connector_attests_it() {
    let harness = harness(4, None).await;
    run(
        &harness,
        two_documents(),
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    let points_before = harness.fake.point_count();
    let refused = run(
        &harness,
        Vec::new(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        refused.status,
        IndexIngestStatusV1::Error as i32,
        "{refused:?}"
    );
    assert!(
        refused.message.contains("returned no documents"),
        "{}",
        refused.message
    );
    assert!(
        refused.message.contains("clean index"),
        "{}",
        refused.message
    );
    assert_eq!(harness.fake.point_count(), points_before);
    // Attested (the artifact bucket): the index follows the source.
    let attested = run(
        &harness,
        Vec::new(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({"fixture_attests_empty": true})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        attested.status,
        IndexIngestStatusV1::Ok as i32,
        "{attested:?}"
    );
    assert_eq!(harness.fake.point_count(), 0);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_first_run_that_keeps_nothing_is_not_a_failure() {
    let harness = harness(4, None).await;
    let first = run(
        &harness,
        two_documents(),
        &command(NS, None),
        &parameters(false, json!({"whitelist": ["*.nothing"]})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(first.status, IndexIngestStatusV1::Ok as i32, "{first:?}");
    assert_eq!(first.indexed_documents, 0);
}

// ---- gap 5: which skips degrade the run -----------------------------------

#[test]
fn only_lost_work_degrades_a_run() {
    for benign in [
        "unsupported_type",
        "empty",
        "too_large",
        "too_many_pages",
        "encrypted",
    ] {
        assert!(!skip_loses_document(benign, false), "{benign}");
        assert!(!skip_loses_document(benign, true), "{benign}");
    }
    // The optional picture step without a model is benign; an explicitly
    // chosen model chunker without one loses its documents.
    for optional in ["no_model", "needs_model"] {
        assert!(!skip_loses_document(optional, false), "{optional}");
        assert!(skip_loses_document(optional, true), "{optional}");
    }
    for lost in [
        "fetch_failed",
        "extract_failed",
        "extract_timeout",
        "embedding_failed",
        "model_failed",
        "model_cap_exceeded",
        "vision_refused",
        "unreadable",
        "a_reason_nobody_classified",
    ] {
        assert!(skip_loses_document(lost, false), "{lost}");
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn skipped_unsupported_and_empty_files_leave_the_run_ok() {
    let harness = harness(4, None).await;
    let mut binary = document("tool.bin", "v1", "");
    binary.bytes = vec![0, 1, 2, 3];
    binary.reference.size = 4;
    let summary = run(
        &harness,
        vec![
            document("a.md", "v1", "# A\n\nalpha text\n"),
            binary,
            document("blank.md", "v1", "   \n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(
        summary.skipped.get("unsupported_type"),
        Some(&1),
        "{summary:?}"
    );
    assert_eq!(summary.skipped.get("empty"), Some(&1), "{summary:?}");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
}

// ---- gap 1: the ado_plans chunking_tool forms ------------------------------

#[tokio::test(flavor = "multi_thread")]
async fn the_ado_plans_form_chunking_tools_index_instead_of_failing() {
    for tool in ["xml", ""] {
        let harness = harness(4, None).await;
        let summary = run(
            &harness,
            vec![document(
                "plans/1/suites/2/7.md",
                "v1",
                "<steps><step id=\"1\"><parameterizedString>open the page</parameterizedString></step></steps>",
            )],
            &command(NS, None),
            &parameters(false, json!({"chunking_tool": tool, "plan_id": 1, "suite_ids": []})),
            &Always,
        )
        .await
        .expect("summary");
        assert_eq!(
            summary.status,
            IndexIngestStatusV1::Ok as i32,
            "{tool:?}: {summary:?}"
        );
        assert_eq!(summary.indexed_documents, 1, "{tool:?}: {summary:?}");
    }
}

// ---- gap 9: chunking_tool is for the base document ---------------------------

#[tokio::test(flavor = "multi_thread")]
async fn an_attachment_keeps_by_extension_chunking_when_chunking_tool_is_set() {
    let harness = harness(4, None).await;
    let mut attachment = document(
        "PROJ-1/attachments/notes.md",
        "v1",
        "# Notes\n\nattachment text\n",
    );
    attachment.metadata.insert(
        "parent_key".to_owned(),
        Value::String("PROJ-1.md".to_owned()),
    );
    let summary = run(
        &harness,
        vec![
            document("PROJ-1.md", "v1", "# Issue\n\nthe base text\n"),
            attachment,
        ],
        &command(NS, None),
        // `proposal` needs a chat model, which this run has none of: the base
        // document cannot be chunked by it, the attachment never was.
        &parameters(false, json!({"chunking_tool": "proposal"})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(
        stored(&harness).await[0].document_key,
        "PROJ-1/attachments/notes.md"
    );
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
    // The model chunker was chosen explicitly: its lost document degrades.
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::PartlyIndexed as i32,
        "{summary:?}"
    );
}

// ---- gap 8: ignored per-page loader settings --------------------------------

#[test]
fn per_page_loader_settings_are_named_when_ignored() {
    let config = json!({
        ".xlsx": {"header_row_number": 2, "sheet_name": "Q1", "max_tokens": 100},
        ".CSV": {"add_header_to_chunks": true},
        "pptx": {"pages_per_chunk": 3, "mode": null},
        ".pdf": {"extract_images": true, "use_llm": false},
        ".md": {"header_row_number": 1},
    });
    assert_eq!(
        ignored_loader_keys(&config),
        [
            ".xlsx.header_row_number",
            ".xlsx.sheet_name",
            ".csv.add_header_to_chunks",
            ".pptx.pages_per_chunk",
            ".pdf.extract_images",
        ]
    );
    assert!(ignored_loader_keys(&Value::Null).is_empty());
    assert!(ignored_loader_keys(&json!({".md": {"max_tokens": 5}})).is_empty());
}

#[tokio::test(flavor = "multi_thread")]
async fn the_summary_names_the_ignored_loader_settings_once() {
    let harness = harness(4, None).await;
    let summary = run(
        &harness,
        vec![document("a.md", "v1", "# A\n\nalpha text\n")],
        &command(NS, None),
        &parameters(
            false,
            json!({"chunking_config": {".xlsx": {"sheet_name": "Q1"}, ".pdf": {"extract_images": true}}}),
        ),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::Ok as i32,
        "{summary:?}"
    );
    assert_eq!(
        summary.message.matches("Ignored chunking settings").count(),
        1,
        "{}",
        summary.message
    );
    assert!(
        summary.message.contains(".xlsx.sheet_name"),
        "{}",
        summary.message
    );
    assert!(
        summary.message.contains(".pdf.extract_images"),
        "{}",
        summary.message
    );
}
