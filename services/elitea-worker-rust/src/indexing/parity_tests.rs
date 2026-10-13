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
use super::pipeline::{Plan, hashed_off_executor, skip_loses_document};
use super::source::{PathFilter, commit_hash, with_code_paths};
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
    // Keys are the FULL object keys (folder included) and every pattern
    // meets that key.
    assert!(docs.keeps("docs/guide.md"));
    assert!(docs.keeps("docs/GUIDE.Md"));
    assert!(docs.keeps("docs/deep/er/report.pdf"));
    assert!(!docs.keeps("docs/draft/idea.md"), "skip wins");
    assert!(!docs.keeps("docs/guide.txt"), "not included");
    assert!(!docs.keeps("docs/scratch.tmp"));
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
fn the_whitelist_blacklist_and_extension_patterns_meet_the_same_full_key() {
    // A folder-qualified pattern means the same thing in all four lists.
    let scoped = filter(
        "artifact",
        json!({
            "folder": "reports",
            "whitelist": ["reports/*"],
            "blacklist": ["reports/old/*"],
            "include_extensions": ["reports/*.md"],
            "skip_extensions": ["reports/*/draft.md"],
        }),
    );
    assert!(scoped.keeps("reports/q1.md"));
    assert!(!scoped.keeps("reports/old/q0.md"), "blacklist");
    assert!(!scoped.keeps("reports/q1/draft.md"), "skip_extensions");
    assert!(!scoped.keeps("reports/q1.csv"), "include_extensions");
    // The folder-relative spelling matches nothing: the key is the full one.
    let relative = filter(
        "artifact",
        json!({"folder": "reports", "whitelist": ["q1.md"], "include_extensions": ["q1.md"]}),
    );
    assert!(!relative.keeps("reports/q1.md"));
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
    let hash = commit_hash(&fetched.bytes);
    let fetched = with_code_paths(fetched);
    assert_eq!(fetched.metadata["filename"], "src/app.py");
    assert_eq!(fetched.metadata["file_path"], "src/app.py");
    // The SDK's `commit_hash` is the SHA-256 of the text.
    assert_eq!(hash, sha256_hex("def main():\n    return 1\n"));
    assert_eq!(hash.len(), 64);
    assert_eq!(
        fetched.metadata["sha"], "blobsha",
        "the connector's keys stay"
    );
    // A key the connector already set is not replaced.
    let mut preset = document("a.py", "v", "x");
    preset
        .metadata
        .insert("filename".to_owned(), Value::String("kept".to_owned()));
    assert_eq!(with_code_paths(preset).metadata["filename"], "kept");
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

    // A clean run is the user's explicit instruction to replace the index:
    // with the same typo it empties it, and says so.
    let clean = run(
        &harness,
        two_documents(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(true, json!({"whitelist": ["*.mdd"]})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(clean.status, IndexIngestStatusV1::Ok as i32, "{clean:?}");
    assert!(clean.message.contains("now empty"), "{}", clean.message);
    assert_eq!(clean.total_documents, Some(0));
    assert_eq!(harness.fake.point_count(), 0);
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
    let changed = Plan::Changed { prior_chunks: 3 };
    for benign in [
        "unsupported_type",
        "empty",
        "too_large",
        "too_many_pages",
        "encrypted",
    ] {
        // A NEW document that is not indexable by design loses nothing...
        assert!(!skip_loses_document(benign, Plan::New, false), "{benign}");
        assert!(!skip_loses_document(benign, Plan::New, true), "{benign}");
        // ... but a stored one keeps its stale points: lost work.
        assert!(skip_loses_document(benign, changed, false), "{benign}");
    }
    // The optional picture step without a model is benign for a new
    // document; a document whose own config asked for a model step loses it.
    for optional in ["no_model", "needs_model"] {
        assert!(
            !skip_loses_document(optional, Plan::New, false),
            "{optional}"
        );
        assert!(skip_loses_document(optional, Plan::New, true), "{optional}");
        assert!(skip_loses_document(optional, changed, false), "{optional}");
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
        assert!(skip_loses_document(lost, Plan::New, false), "{lost}");
        assert!(skip_loses_document(lost, changed, false), "{lost}");
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

// ---- review round 1 ---------------------------------------------------------

#[tokio::test(flavor = "multi_thread")]
async fn the_new_default_file_type_filter_keeps_stored_documents_until_a_clean_run() {
    let harness = harness(4, None).await;
    let both = || {
        vec![
            document("a.md", "v1", "# A\n\nalpha text\n"),
            document("notes.rst", "v1", "logged words here\n"),
        ]
    };
    // An earlier run (before the allowlist existed) indexed both.
    let first = run(
        &harness,
        both(),
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    assert_eq!(first.indexed_documents, 2, "{first:?}");
    let points_before = harness.fake.point_count();

    // Now the allowlist applies: `.rst` is out of scope. It is neither
    // re-indexed nor deleted, while it stays in the source or after it left.
    for listing in [both(), vec![document("a.md", "v1", "# A\n\nalpha text\n")]] {
        let again = run(
            &harness,
            listing,
            &command(NS, Some((super::tests::MODEL, 4))),
            &parameters(false, json!({"fixture_skip_unsupported": true})),
            &Always,
        )
        .await
        .expect("summary");
        assert_eq!(again.status, IndexIngestStatusV1::Ok as i32, "{again:?}");
        assert!(again.removed_document_keys.is_empty(), "{again:?}");
        // The kept document is surfaced, not silent.
        assert!(
            again.message.contains(
                "1 stored documents are outside the current file-type filter and were left unchanged; run with clean_index to drop them"
            ),
            "{}",
            again.message
        );
        assert_eq!(again.total_documents, Some(2), "{again:?}");
        assert!(!again.whole_index_record, "the kept document is unnamed");
        assert_eq!(harness.fake.point_count(), points_before);
        assert_eq!(stored(&harness).await.len(), 2);
    }

    // A clean run applies the current scope.
    let clean = run(
        &harness,
        both(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(true, json!({"fixture_skip_unsupported": true})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(clean.status, IndexIngestStatusV1::Ok as i32, "{clean:?}");
    let keys: Vec<String> = stored(&harness)
        .await
        .into_iter()
        .map(|point| point.document_key)
        .collect();
    assert_eq!(keys, vec!["a.md"]);
}

/// An empty source (an empty JQL result, a branch with no files) empties
/// the index only on a clean run, an explicit user action; an incremental
/// run is protected and the index stays intact.
#[tokio::test(flavor = "multi_thread")]
async fn an_empty_source_empties_the_index_only_on_a_clean_run() {
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
    assert_eq!(refused.status, IndexIngestStatusV1::Error as i32);
    assert!(
        refused.message.contains("clean_index"),
        "{}",
        refused.message
    );
    assert_eq!(harness.fake.point_count(), points_before);
    assert_eq!(stored(&harness).await.len(), 2, "the index is intact");

    let emptied = run(
        &harness,
        Vec::new(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(true, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        emptied.status,
        IndexIngestStatusV1::Ok as i32,
        "{emptied:?}"
    );
    assert!(emptied.message.contains("now empty"), "{}", emptied.message);
    assert_eq!(emptied.total_documents, Some(0));
    assert_eq!(emptied.total_chunks, Some(0));
    assert!(emptied.whole_index_record, "{emptied:?}");
    assert_eq!(harness.fake.point_count(), 0);

    // A clean run over an index that was empty already is no failure.
    let again = run(
        &harness,
        Vec::new(),
        &command(NS, None),
        &parameters(true, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(again.status, IndexIngestStatusV1::Ok as i32, "{again:?}");
}

/// A clean run reads nothing of the index, so one that outgrew what a run
/// can diff against does not fail it: the clean run rebuilds it.
#[tokio::test(flavor = "multi_thread")]
async fn an_oversized_index_is_rebuilt_by_a_clean_run() {
    let mut harness = harness(4, None).await;
    run(
        &harness,
        vec![
            document("a.md", "v1", "# A\n\nalpha text\n"),
            document("b.md", "v1", "# B\n\nbravo text\n"),
            document("c.md", "v1", "# C\n\ncharlie text\n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    // The run now handles one document: the three stored versions are over
    // twice that.
    harness.runtime.limits.max_documents = 1;
    let refused = run(
        &harness,
        vec![document("a.md", "v1", "# A\n\nalpha text\n")],
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(refused.status, IndexIngestStatusV1::Error as i32);
    assert!(
        refused.message.contains("document versions"),
        "{}",
        refused.message
    );
    assert_eq!(stored(&harness).await.len(), 3);

    let rebuilt = run(
        &harness,
        vec![document("a.md", "v2", "# A\n\nalpha text again\n")],
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(true, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(
        rebuilt.status,
        IndexIngestStatusV1::Ok as i32,
        "{rebuilt:?}"
    );
    assert_eq!(rebuilt.indexed_documents, 1);
    let keys: Vec<String> = stored(&harness)
        .await
        .into_iter()
        .map(|point| point.document_key)
        .collect();
    assert_eq!(keys, vec!["a.md"]);
}

#[tokio::test(flavor = "multi_thread")]
async fn an_explicit_use_llm_without_a_model_degrades_the_run_and_the_default_does_not() {
    for (extra, expected) in [
        (json!({}), IndexIngestStatusV1::Ok),
        (
            json!({"chunking_config": {"use_llm": false}}),
            IndexIngestStatusV1::Ok,
        ),
        (
            json!({"chunking_config": {"use_llm": true}}),
            IndexIngestStatusV1::PartlyIndexed,
        ),
        (
            json!({"chunking_config": {".png": {"use_llm": true}}}),
            IndexIngestStatusV1::PartlyIndexed,
        ),
    ] {
        let harness = harness(4, None).await;
        let summary = run(
            &harness,
            vec![
                document("a.md", "v1", "# A\n\nalpha text\n"),
                super::tests::png_document("logo.png"),
            ],
            &command(NS, None),
            &parameters(false, extra.clone()),
            &Always,
        )
        .await
        .expect("summary");
        assert_eq!(summary.indexed_documents, 1, "{extra}: {summary:?}");
        assert_eq!(summary.status, expected as i32, "{extra}: {summary:?}");
    }
}

#[test]
fn commit_hash_hashes_valid_utf8_as_it_is_and_drops_invalid_bytes() {
    let hash_of = |bytes: &[u8]| commit_hash(bytes);
    // A real U+FFFD in valid UTF-8 stays in the hashed text.
    assert_eq!(hash_of("a\u{FFFD}b".as_bytes()), sha256_hex("a\u{FFFD}b"));
    assert_ne!(hash_of("a\u{FFFD}b".as_bytes()), sha256_hex("ab"));
    // Python's `decode("utf-8", errors="ignore")` drops invalid sequences.
    assert_eq!(hash_of(b"a\xFFb\xE2\x82"), sha256_hex("ab"));
    assert_eq!(hash_of(b"a\xC3\xA9\x80"), sha256_hex("a\u{e9}"));
    assert_eq!(hash_of(b""), sha256_hex(""));
}

#[test]
fn a_dependent_document_config_drops_only_the_base_chunker() {
    let config = elitea_doc_chunk::ChunkingConfig::from_value(
        &json!({"chunking_tool": "markdown", ".md": {"chunker": "text"}}),
    )
    .expect("config");
    let dependent = super::pipeline::dependent_chunking(&config);
    assert_eq!(dependent.base.chunker, None);
    assert_eq!(
        dependent.by_extension[".md"].chunker.as_deref(),
        Some("text"),
        "the per-extension settings stay"
    );
}

/// The artifact object API, scripted: a list under a prefix, and the bytes.
struct Objects {
    objects: Vec<(&'static str, &'static str)>,
    requests: std::sync::Mutex<Vec<String>>,
}

#[async_trait::async_trait]
impl elitea_connectors::transport::Transport for Objects {
    async fn execute(
        &self,
        request: elitea_connectors::transport::Request,
    ) -> Result<elitea_connectors::transport::Response, elitea_connectors::transport::TransportError>
    {
        use elitea_connectors::transport::{HeaderMap, HeaderValue, Response, StatusCode};
        let path = request.url().path().to_owned();
        self.requests.lock().expect("lock").push(path.clone());
        let mut headers = HeaderMap::new();
        let bucket_root = "/api/v2/artifacts/objects/1/docs";
        if path == bucket_root {
            let prefix = request
                .url()
                .query_pairs()
                .find(|(name, _)| name == "prefix")
                .map(|(_, value)| value.into_owned())
                .unwrap_or_default();
            let rows: Vec<Value> = self
                .objects
                .iter()
                .filter(|(key, _)| key.starts_with(&prefix))
                .map(|(key, text)| json!({"key": key, "size_bytes": text.len(), "etag": "\"same-etag\""}))
                .collect();
            headers.insert("content-type", HeaderValue::from_static("application/json"));
            return Ok(Response::from_bytes(
                StatusCode::OK,
                headers,
                json!({"objects": rows}).to_string().into_bytes(),
            ));
        }
        let key = path
            .trim_start_matches(&format!("{bucket_root}/"))
            .to_owned();
        let text = self
            .objects
            .iter()
            .find(|(candidate, _)| *candidate == key)
            .map(|(_, text)| *text)
            .expect("a listed object");
        headers.insert("etag", HeaderValue::from_static("\"same-etag\""));
        Ok(Response::from_bytes(
            StatusCode::OK,
            headers,
            text.as_bytes().to_vec(),
        ))
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn artifact_documents_keep_the_full_key_so_two_folders_do_not_collide() {
    let mut harness = harness(4, None).await;
    let transport = std::sync::Arc::new(Objects {
        objects: vec![
            ("a/readme.md", "# A\n\nfrom folder a\n"),
            ("b/readme.md", "# B\n\nfrom folder b\n"),
        ],
        requests: std::sync::Mutex::new(Vec::new()),
    });
    harness.runtime.transport = transport.clone();
    let configuration = json!({"id": 7, "type": "artifact", "toolkit_name": "files", "settings": {"bucket": "docs"}});
    let now = || 1_700_000_000_000_i64;
    let mut last = None;
    for (n, folder) in ["a", "b"].into_iter().enumerate() {
        let expected = (n > 0).then_some((super::tests::MODEL, 4));
        let summary = super::run_index(
            &harness.runtime,
            super::IndexRun {
                command: &command(NS, expected),
                toolkit_configuration: configuration.to_string().as_bytes(),
                tool_parameters:
                    json!({"index_name": "docs", "clean_index": false, "folder": folder})
                        .to_string()
                        .as_bytes(),
                parsed: None,
                embedding_binding: Some(&super::tests::binding()),
                llm_model: None,
                llm_configuration: None,
                chat_entries: None,
                vector_token: Some(&harness.token),
                context: &harness.context,
                lease: &Always,
                deadline_unix_millis: i64::MAX,
                now_unix_millis: &now,
            },
        )
        .await
        .expect("summary");
        assert_eq!(summary.indexed_documents, 1, "{folder}: {summary:?}");
        last = Some(summary);
    }
    // The two objects share name and etag; only the full key tells them
    // apart, so run b indexed its own document (it was not "unchanged") and
    // the index follows the folder it was run over.
    let keys: Vec<String> = stored(&harness)
        .await
        .into_iter()
        .map(|point| point.document_key)
        .collect();
    assert_eq!(keys, vec!["b/readme.md"]);
    let last = last.expect("ran");
    assert_eq!(last.removed_document_keys, vec!["a/readme.md"]);
    assert_eq!(last.documents[0].document_key, "b/readme.md");
    // The folder is the list prefix; the fetch asks for the full key.
    let requests = transport.requests.lock().expect("lock").clone();
    assert!(
        requests
            .iter()
            .any(|path| path.ends_with("/docs/a/readme.md")),
        "{requests:?}"
    );
}

// ---- review round 2 ---------------------------------------------------------

/// A skip that is benign for a NEW document is lost work for a CHANGED one:
/// its old points stay, stale.
#[tokio::test(flavor = "multi_thread")]
async fn a_benign_skip_of_a_changed_stored_document_degrades_the_run() {
    let mut harness = harness(4, None).await;
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
    // `a.md` changed to blank text (`empty`); `b.py` changed to a file over
    // the size limit (`too_large`).
    harness.runtime.limits.max_document_bytes = 64;
    let mut big = document("b.py", "v2", &"x = 1\n".repeat(40));
    big.reference.size = big.bytes.len() as u64;
    let second = run(
        &harness,
        vec![document("a.md", "v2", "   \n"), big],
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(second.skipped.get("empty"), Some(&1), "{second:?}");
    assert_eq!(second.skipped.get("too_large"), Some(&1), "{second:?}");
    assert_eq!(
        second.status,
        IndexIngestStatusV1::PartlyIndexed as i32,
        "{second:?}"
    );
    // The stale versions are still in the index, and the totals say so.
    assert_eq!(harness.fake.point_count(), points_before);
    assert_eq!(second.total_documents, Some(2), "{second:?}");
    assert!(!second.whole_index_record);

    // The same two skips on a first run are by-design and leave it OK.
    let fresh = super::tests::harness(4, None).await;
    let mut limited = fresh;
    limited.runtime.limits.max_document_bytes = 64;
    let mut big = document("b.py", "v1", &"x = 1\n".repeat(40));
    big.reference.size = big.bytes.len() as u64;
    let first = run(
        &limited,
        vec![document("a.md", "v1", "   \n"), big],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(first.skipped.get("empty"), Some(&1), "{first:?}");
    assert_eq!(first.skipped.get("too_large"), Some(&1), "{first:?}");
    assert_eq!(first.status, IndexIngestStatusV1::Ok as i32, "{first:?}");
}

/// A `too_large` that comes from the embedding model's request bound is a
/// rejection of the document, not a limit of this worker's: lost work even
/// for a new document.
#[tokio::test(flavor = "multi_thread")]
async fn an_embedding_request_over_the_bound_degrades_the_run() {
    let harness = harness(4, None).await;
    // One JSON object kept whole (`max_chunk_size`) is a single chunk of
    // over 8 MiB, beyond the embeddings request bound.
    let huge = format!("{{\"k\": \"{}\"}}", "x".repeat(9 * 1024 * 1024));
    let summary = run(
        &harness,
        vec![
            document("big.json", "v1", &huge),
            document("a.md", "v1", "# A\n\nalpha text\n"),
        ],
        &command(NS, None),
        &parameters(
            false,
            json!({"chunking_config": {"max_chunk_size": 100_000_000}}),
        ),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(summary.skipped.get("too_large"), Some(&1), "{summary:?}");
    assert_eq!(
        summary.status,
        IndexIngestStatusV1::PartlyIndexed as i32,
        "{summary:?}"
    );
}

/// Whether a missing model degrades the run is the DOCUMENT's own config,
/// not the config as a whole.
#[tokio::test(flavor = "multi_thread")]
async fn a_missing_model_degrades_only_the_documents_that_asked_for_one() {
    let docs = || {
        vec![
            document("a.py", "v1", "def a():\n    return 1\n"),
            super::tests::png_document("logo.png"),
        ]
    };
    // `.md` selects the proposal chunker, no `.md` is listed, and the
    // default-on picture step of `logo.png` stays optional: OK.
    let harness_md = harness(4, None).await;
    let optional = run(
        &harness_md,
        docs(),
        &command(NS, None),
        &parameters(
            false,
            json!({"chunking_config": {".md": {"chunker": "proposal"}}}),
        ),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(optional.skipped.get("no_model"), Some(&1), "{optional:?}");
    assert_eq!(
        optional.status,
        IndexIngestStatusV1::Ok as i32,
        "{optional:?}"
    );

    // The same config with a `.md` document: that document explicitly needs
    // the model, and its skip degrades the run.
    let harness_both = super::tests::harness(4, None).await;
    let mut with_md = docs();
    with_md.push(document("README.md", "v1", "# Title\n\nsome words\n"));
    let asked = run(
        &harness_both,
        with_md,
        &command(NS, None),
        &parameters(
            false,
            json!({"chunking_config": {".md": {"chunker": "proposal"}}}),
        ),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(asked.skipped.get("no_model"), Some(&2), "{asked:?}");
    assert_eq!(
        asked.status,
        IndexIngestStatusV1::PartlyIndexed as i32,
        "{asked:?}"
    );

    // The picture that opted in, and only it, degrades.
    for (config, expected) in [
        (
            json!({".png": {"use_llm": true}}),
            IndexIngestStatusV1::PartlyIndexed,
        ),
        (json!({".jpg": {"use_llm": true}}), IndexIngestStatusV1::Ok),
    ] {
        let h = super::tests::harness(4, None).await;
        let summary = run(
            &h,
            docs(),
            &command(NS, None),
            &parameters(false, json!({"chunking_config": config})),
            &Always,
        )
        .await
        .expect("summary");
        assert_eq!(summary.status, expected as i32, "{config}: {summary:?}");
    }
}

/// A repository that now lists only files outside the file-type allowlist:
/// an incremental run refuses (the in-scope index would be deleted), a clean
/// run empties the index of them.
#[tokio::test(flavor = "multi_thread")]
async fn a_repository_listing_only_excluded_files_is_refused_or_emptied_by_a_clean_run() {
    let harness = harness(4, None).await;
    let rst = || vec![document("notes.rst", "v1", "logged words here\n")];
    run(
        &harness,
        vec![
            document("a.md", "v1", "# A\n\nalpha text\n"),
            document("notes.rst", "v1", "logged words here\n"),
        ],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    let points_before = harness.fake.point_count();

    let refused = run(
        &harness,
        rst(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({"fixture_skip_unsupported": true})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(refused.status, IndexIngestStatusV1::Error as i32);
    assert!(refused.message.contains("file-type"), "{}", refused.message);
    assert_eq!(harness.fake.point_count(), points_before);

    let clean = run(
        &harness,
        rst(),
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(true, json!({"fixture_skip_unsupported": true})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(clean.status, IndexIngestStatusV1::Ok as i32, "{clean:?}");
    assert!(clean.message.contains("now empty"), "{}", clean.message);
    assert_eq!(clean.total_documents, Some(0));
    assert_eq!(harness.fake.point_count(), 0);
}

/// An incremental run over an index that holds only out-of-scope documents
/// has nothing to protect and nothing to delete: it keeps and names them.
#[tokio::test(flavor = "multi_thread")]
async fn stored_documents_outside_the_filter_are_counted_and_named_not_deleted() {
    let harness = harness(4, None).await;
    run(
        &harness,
        vec![document("notes.rst", "v1", "logged words here\n")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("first");
    let points_before = harness.fake.point_count();
    let second = run(
        &harness,
        vec![document("a.md", "v1", "# A\n\nalpha text\n")],
        &command(NS, Some((super::tests::MODEL, 4))),
        &parameters(false, json!({"fixture_skip_unsupported": true})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(second.status, IndexIngestStatusV1::Ok as i32, "{second:?}");
    assert!(
        second
            .message
            .contains("1 stored documents are outside the current file-type filter"),
        "{}",
        second.message
    );
    assert_eq!(second.total_documents, Some(2), "{second:?}");
    assert!(second.removed_document_keys.is_empty());
    assert!(harness.fake.point_count() > points_before);
}

/// The commit hash is computed off the async executor on every route a
/// repository file takes, and the bytes come back untouched.
#[tokio::test(flavor = "multi_thread")]
async fn the_commit_hash_is_computed_on_a_blocking_thread_and_returns_the_bytes() {
    let (bytes, hash) = hashed_off_executor(b"a\xFFb".to_vec())
        .await
        .expect("hashed");
    assert_eq!(bytes, b"a\xFFb");
    assert_eq!(hash, sha256_hex("ab"));

    // A fixture that is a repository: text, picture and (no sidecar here)
    // every route runs the hash step without disturbing the run.
    let harness = harness(4, None).await;
    let mut text = document("a.md", "v1", "# A\n\nalpha text\n");
    text.metadata
        .insert("fixture_hash_commit".to_owned(), Value::Bool(true));
    let summary = run(
        &harness,
        vec![text, super::tests::png_document("logo.png")],
        &command(NS, None),
        &parameters(false, json!({})),
        &Always,
    )
    .await
    .expect("summary");
    assert_eq!(summary.indexed_documents, 1, "{summary:?}");
    assert_eq!(summary.skipped.get("no_model"), Some(&1), "{summary:?}");
}
