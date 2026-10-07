//! The page phase against the frozen generation contract
//! (`conformance/provider/fixtures/deepwiki/generation`): the names, the
//! bytes and the fields the Go host and the web app read.
//!
//! `wiki_manifest.json` is RESTATED by hand (its commit prefix has seven
//! characters where the identifier rule takes eight), so the manifest is
//! checked field by field against the rule, the key order against the
//! fixture minus the keys ADR-0026 removes.

use elitea_deepwiki_engine::storage::build::WikiRecord;
use elitea_deepwiki_engine::wiki::compose::{
    ComposeInput, Instant, VersionClock, build_repo_identifier, compose, normalize_wiki_id,
    rebase_artifact_name,
};
use elitea_deepwiki_engine::wiki::export::{ArtifactExporter, safe_filename};
use elitea_deepwiki_engine::wiki::spec::{
    PageSpec, PageStatus, SectionSpec, WikiPage, WikiStructureSpec,
};
use serde_json::{Map, Value};
use std::path::PathBuf;
use std::time::{Duration, UNIX_EPOCH};

fn fixture(name: &str) -> Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../conformance/provider/fixtures/deepwiki/generation")
        .join(name);
    serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap()
}

/// The ADR-0026 removals from the manifest.
const OMITTED: [&str; 10] = [
    "faiss_cache_key",
    "graph_cache_key",
    "docstore_cache_key",
    "docstore_files",
    "bm25_cache_key",
    "bm25_files",
    "unified_db_key",
    "unified_db_files",
    "graph_files",
    "faiss_files",
];

fn spec(name: &str) -> PageSpec {
    PageSpec {
        page_name: name.into(),
        page_order: 1,
        description: String::new(),
        content_focus: String::new(),
        rationale: String::new(),
        target_symbols: Vec::new(),
        target_docs: Vec::new(),
        target_folders: Vec::new(),
        key_files: Vec::new(),
        retrieval_query: String::new(),
        metadata: Map::new(),
    }
}

/// The fixture wiki: the titles of `page_names.json`, the bodies of
/// `wiki_structure.json`.
fn fixture_wiki() -> (WikiStructureSpec, Vec<WikiPage>) {
    let names = fixture("page_names.json");
    let body = fixture("wiki_structure.json")["body"].clone();
    let mut sections: Vec<SectionSpec> = Vec::new();
    let mut pages = Vec::new();
    for page in names["pages"].as_array().unwrap() {
        let section = page["section"].as_str().unwrap();
        let title = page["title"].as_str().unwrap();
        if sections.last().is_none_or(|s| s.section_name != section) {
            sections.push(SectionSpec {
                section_name: section.into(),
                section_order: 1,
                description: String::new(),
                rationale: String::new(),
                pages: Vec::new(),
            });
        }
        let section_index = sections.len() - 1;
        let page_index = sections[section_index].pages.len();
        sections[section_index].pages.push(spec(title));
        let content = body["sections"][section_index]["pages"][page_index]["page_content"]
            .as_str()
            .unwrap();
        pages.push(WikiPage {
            page_id: format!("{section_index}#{page_index}"),
            title: title.into(),
            content: content.into(),
            status: PageStatus::Completed,
        });
    }
    let structure = WikiStructureSpec {
        wiki_title: body["wiki_title"].as_str().unwrap().into(),
        overview: "Overview".into(),
        sections,
        total_pages: 5,
    };
    (structure, pages)
}

#[test]
fn the_structure_artifact_is_the_fixtures_bytes() {
    let (structure, pages) = fixture_wiki();
    let fixture = fixture("wiki_structure.json");
    let wiki_id = "acme--notes-service--main";
    let exporter = ArtifactExporter {
        wiki_id: Some(wiki_id.into()),
    };
    let artifacts = exporter.export(&structure, &pages, "TIMESTAMP").unwrap();
    let json = &artifacts[0];
    assert_eq!(json.name, fixture["artifact"]["name"].as_str().unwrap());
    assert_eq!(json.mime, fixture["artifact"]["type"].as_str().unwrap());
    assert_eq!(json.data, fixture["artifact"]["data"].as_str().unwrap());
}

#[test]
fn page_names_follow_the_slug_and_rebase_rules() {
    let names = fixture("page_names.json");
    let wiki_id = names["wiki_id"].as_str().unwrap();
    assert_eq!(
        normalize_wiki_id("acme/notes-service:main:0000000"),
        wiki_id
    );
    let mut manifest_pages = Vec::new();
    for page in names["pages"].as_array().unwrap() {
        let raw = format!(
            "wiki_pages/{}/{}.md",
            safe_filename(page["section"].as_str().unwrap()).unwrap(),
            safe_filename(page["title"].as_str().unwrap()).unwrap()
        );
        assert_eq!(raw, page["raw_name"].as_str().unwrap());
        let rebased = rebase_artifact_name(&raw, wiki_id, "wiki_pages");
        assert_eq!(rebased, page["artifact_name"].as_str().unwrap());
        manifest_pages.push(rebased);
    }
    let expected: Vec<String> = serde_json::from_value(names["manifest_pages"].clone()).unwrap();
    assert_eq!(manifest_pages, expected);
}

fn composed_fixture_result() -> (Map<String, Value>, String) {
    let (structure, pages) = fixture_wiki();
    let exporter = ArtifactExporter {
        wiki_id: Some("acme--notes-service--main".into()),
    };
    let artifacts = exporter
        .export(&structure, &pages, "20260101_000000")
        .unwrap();
    let repository_context = fixture("composed_result.json")["result_objects"]
        .as_array()
        .unwrap()
        .iter()
        .find(|o| o["object_type"] == "repository_context")
        .unwrap()["data"]
        .as_str()
        .unwrap()
        .to_owned();
    let clock = VersionClock {
        now: Instant::utc(UNIX_EPOCH + Duration::from_hours(490_896)),
        uuid8: "0000000a".into(),
    };
    let composed = compose(
        ComposeInput {
            repository: "acme/notes-service".into(),
            canonical_repository: "acme/notes-service".into(),
            branch: "main".into(),
            commit_hash: Some("0000000deadbeef0000000deadbeef0000000000".into()),
            provider_type: "github".into(),
            indexing_method: "filesystem".into(),
            repository_context,
            artifacts,
            errors: Vec::new(),
            failed_pages: Vec::new(),
            message: "Wiki generated: 5 pages across 2 sections".into(),
            execution_time: 1.0,
        },
        &clock,
    );
    let manifest = composed.result["artifacts"]
        .as_array()
        .unwrap()
        .iter()
        .find(|a| a["object_type"] == "wiki_manifest")
        .unwrap()["data"]
        .as_str()
        .unwrap()
        .to_owned();
    (composed.result, manifest)
}

#[test]
fn the_manifest_keeps_the_contract_minus_the_index_files() {
    let fixture = fixture("wiki_manifest.json");
    let body = fixture["body"].as_object().unwrap();
    let (result, manifest_text) = composed_fixture_result();
    let manifest: Map<String, Value> = serde_json::from_str(&manifest_text).unwrap();
    let expected_keys: Vec<&String> = body
        .keys()
        .filter(|k| !OMITTED.contains(&k.as_str()))
        .collect();
    let keys: Vec<&String> = manifest.keys().collect();
    assert_eq!(keys, expected_keys);
    for key in [
        "schema_version",
        "wiki_id",
        "wiki_title",
        "description",
        "wiki_version_id",
        "created_at",
        "repository",
        "branch",
        "commit_hash",
        "provider_type",
    ] {
        assert_eq!(manifest[key], body[key], "{key}");
    }
    // The identifier rule (commit[:8]); the restated fixture shows seven.
    let identifier = build_repo_identifier(
        "acme/notes-service",
        "main",
        Some("0000000deadbeef0000000deadbeef0000000000"),
    );
    assert_eq!(identifier, "acme/notes-service:main:0000000d");
    assert_eq!(manifest["canonical_repo_identifier"], identifier.as_str());
    let analysis_key = format!("{identifier}@20260101T000000Z-0000000a");
    assert_eq!(manifest["analysis_key"], analysis_key.as_str());
    assert_eq!(manifest["analysis_cache_key"].as_str().unwrap().len(), 32);
    // indent=2, ensure_ascii=False.
    assert!(manifest_text.starts_with("{\n  \"schema_version\": 2,\n  \"wiki_id\": "));
    assert_eq!(
        result["artifacts"].as_array().unwrap().last().unwrap()["name"],
        fixture["artifact_name"].as_str().unwrap()
    );
    let pages: Vec<String> = serde_json::from_value(manifest["pages"].clone()).unwrap();
    let contract: Vec<String> = serde_json::from_value(body["pages"].clone()).unwrap();
    // The engine's README index is a page too (Python's export lists it).
    assert_eq!(pages[0], "acme--notes-service--main/wiki_pages/README.md");
    assert_eq!(pages[1..], contract[..]);
}

#[test]
fn the_page_artifacts_are_the_composed_objects() {
    let (result, _) = composed_fixture_result();
    let composed = fixture("composed_result.json");
    let artifacts = result["artifacts"].as_array().unwrap();
    for object in composed["result_objects"].as_array().unwrap() {
        let object_type = object["object_type"].as_str().unwrap();
        if object_type != "wiki_page" && object_type != "wiki_structure" {
            continue;
        }
        let name = object["name"]
            .as_str()
            .unwrap()
            .replace("TIMESTAMP", "20260101_000000");
        let found = artifacts.iter().find(|a| a["name"] == name.as_str());
        let found = found.unwrap_or_else(|| panic!("no artifact {name}"));
        assert_eq!(found["data"], object["data"], "{name}");
    }
}

#[test]
fn the_registry_row_reads_the_composed_fields() {
    let (result, _) = composed_fixture_result();
    let entry = fixture("registry_entry.json")["entry"].clone();
    let record = WikiRecord::from_result(&Value::Object(result));
    assert_eq!(record.repo.as_deref(), entry["repo"].as_str());
    assert_eq!(record.branch.as_deref(), entry["branch"].as_str());
    assert_eq!(record.provider.as_deref(), entry["provider"].as_str());
    assert_eq!(record.host.as_deref(), entry["host"].as_str());
    assert_eq!(
        record.display_name.as_deref(),
        entry["display_name"].as_str()
    );
    assert_eq!(record.description.as_deref(), entry["description"].as_str());
    assert_eq!(record.folder_path.as_deref(), entry["folder_path"].as_str());
    assert_eq!(record.commit_hash.as_deref(), entry["commit_hash"].as_str());
}
