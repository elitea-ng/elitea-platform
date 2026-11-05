//! The contract's own tests: the schema snapshot, the snapshot grammar
//! goldens, and the conformance kit run against the reference driver in
//! `common/`.
//!
//! `tests/golden/schemas.json` pins every tool's name, kind, description and
//! schema: a change to the contract is a reviewed diff of that file.
//! Regenerate with `UPDATE_GOLDEN=1 cargo test -p elitea-browser-tools`.

mod common;

use std::path::PathBuf;

use common::{FakeDriver, url_for};
use elitea_browser_tools::conformance::{self, Config, FIXTURES};
use elitea_browser_tools::snapshot::Snapshot;
use elitea_browser_tools::{SnapshotId, ToolOutput, dispatch, frame, spec};
use serde_json::{Value, json};

fn golden_path(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/golden")
        .join(name)
}

#[test]
fn schema_snapshot() {
    let actual = serde_json::to_string_pretty(&spec::schema_snapshot()).unwrap() + "\n";
    let path = golden_path("schemas.json");
    if std::env::var_os("UPDATE_GOLDEN").is_some() {
        std::fs::write(&path, &actual).unwrap();
    }
    let expected = std::fs::read_to_string(&path).unwrap();
    assert_eq!(
        actual, expected,
        "the browser tool contract changed: review it, then UPDATE_GOLDEN=1"
    );
}

#[test]
fn goldens_are_the_grammar() {
    let id = SnapshotId::sequence(1);
    for fixture in FIXTURES {
        let parsed = Snapshot::parse_tree(&id, fixture.golden)
            .unwrap_or_else(|error| panic!("{}: {error}", fixture.name));
        assert_eq!(parsed.render_tree(), fixture.golden, "{}", fixture.name);
        // Refs are e1..eN in document order.
        let mut refs = Vec::new();
        for line in fixture.golden.lines() {
            if let Some((_, rest)) = line.split_once("[ref=e") {
                refs.push(rest.split(']').next().unwrap().parse::<u32>().unwrap());
            }
        }
        let expected: Vec<u32> = (1..=u32::try_from(refs.len()).unwrap()).collect();
        assert_eq!(refs, expected, "{}", fixture.name);
        for secret in conformance::SECRET_VALUES {
            assert!(!fixture.golden.contains(secret), "{}", fixture.name);
        }
    }
}

#[test]
fn fixtures_exist_on_disk() {
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("conformance");
    let on_disk = |dir: &str, extension: &str| {
        let mut names: Vec<String> = std::fs::read_dir(root.join(dir))
            .unwrap()
            .filter_map(|entry| {
                let path = entry.unwrap().path();
                (path.extension()? == extension)
                    .then(|| path.file_stem()?.to_str().map(str::to_owned))?
            })
            .collect();
        names.sort();
        names
    };
    let mut listed: Vec<String> = FIXTURES.iter().map(|f| f.name.to_owned()).collect();
    listed.sort();
    assert_eq!(on_disk("fixtures", "html"), listed);
    assert_eq!(on_disk("golden", "snapshot"), listed);
    for fixture in FIXTURES {
        assert!(
            fixture
                .html
                .contains(&format!("<title>{}</title>", fixture.title)),
            "{}",
            fixture.name
        );
    }
}

#[tokio::test]
async fn reference_driver_passes_the_kit() {
    let driver = FakeDriver::new();
    let config = Config {
        url_for: &url_for,
        differences: Vec::new(),
    };
    let report = conformance::run(&driver, &config).await;
    assert!(report.failures().is_empty(), "{report}");
    assert!(report.checks.len() > 30, "{report}");
}

#[tokio::test]
async fn a_driver_that_types_into_secure_fields_fails_the_kit() {
    let mut driver = FakeDriver::new();
    driver.ignores_secure_fields = true;
    let config = Config {
        url_for: &url_for,
        differences: Vec::new(),
    };
    let report = conformance::run(&driver, &config).await;
    let failed: Vec<&str> = report
        .failures()
        .iter()
        .map(|check| check.step.as_str())
        .collect();
    assert_eq!(
        failed,
        [
            "password is secure_field_refused",
            "Card number is secure_field_refused",
            "Security code is secure_field_refused",
            "New password is secure_field_refused",
        ]
    );
}

#[tokio::test]
async fn a_listed_difference_replaces_the_golden() {
    let driver = FakeDriver::new();
    let config = Config {
        url_for: &url_for,
        differences: vec![conformance::Difference {
            fixture: "iframes",
            tree: "- heading \"Frames\" [level=1]\n".to_owned(),
            reason: "test",
        }],
    };
    let report = conformance::run(&driver, &config).await;
    let failed: Vec<(&str, &str)> = report
        .failures()
        .iter()
        .map(|check| (check.fixture, check.step.as_str()))
        .collect();
    assert_eq!(failed, [("iframes", "snapshot matches the golden")]);
}

#[tokio::test]
async fn dispatch_shapes() {
    let driver = FakeDriver::new();
    let forms = &FIXTURES[0];
    let opened = dispatch(&driver, "browser_open", &json!({ "url": url_for(forms) })).await;
    let opened = opened.result();
    assert_eq!(opened["status"], "ok");
    assert_eq!(opened["refs_stale"], true);
    let (header, nonce) = frame::unframe(opened["content"].as_str().unwrap()).unwrap();
    assert_eq!(
        header,
        "# url \"http://127.0.0.1:9/forms.html\"\n# title \"Forms\"\n"
    );

    let snapshot = dispatch(&driver, "browser_snapshot", &json!({ "max_nodes": 3 })).await;
    let snapshot = snapshot.result();
    assert_eq!(snapshot["truncated"], true);
    assert_eq!(snapshot["omitted"], 9);
    assert_eq!(snapshot["snapshot"], "s1");
    let (inner, second) = frame::unframe(snapshot["content"].as_str().unwrap()).unwrap();
    assert_ne!(nonce, second, "a nonce per call");
    assert_eq!(
        inner,
        "# snapshot s1\n\
         # url \"http://127.0.0.1:9/forms.html\"\n\
         # title \"Forms\"\n\
         - main:\n  - heading \"Sign in\" [level=1]\n  - form \"Sign in\"\n\
         # omitted 9\n"
    );

    match dispatch(&driver, "browser_screenshot", &Value::Null).await {
        ToolOutput::Image { result, .. } => assert_eq!(result["media_type"], "image/png"),
        other @ ToolOutput::Json(_) => panic!("{other:?}"),
    }
    let asked = dispatch(
        &driver,
        "browser_request_user",
        &json!({ "reason": "sign in" }),
    )
    .await;
    assert_eq!(asked.result()["code"], "no_person");
    let evaluate = dispatch(&driver, "browser_evaluate", &json!({ "script": "1" })).await;
    assert_eq!(evaluate.result()["code"], "unknown_tool");
}
