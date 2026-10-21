//! The parsers over recorded runner output
//! (`tests/fixtures/reports/`: real cargo and go runs of the fixture
//! projects, paths rewritten to `/work/…`; vitest, jest, pytest and
//! Surefire reports in their documented shapes).

use std::path::Path;

use super::{Failure, MAX_MESSAGE_BYTES, bounded, cargo, js_json, needs_network, pytest_text};

macro_rules! recorded {
    ($name:literal) => {
        include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/fixtures/reports/",
            $name
        ))
    };
}

fn at(failure: &Failure) -> (Option<&str>, Option<u32>) {
    (failure.file.as_deref(), failure.line)
}

#[test]
fn cargo_json() {
    let root = Some(Path::new("/work/fixture"));
    let report = cargo(
        recorded!("cargo_test.stdout"),
        recorded!("cargo_test.stderr"),
        root,
    );
    assert!(report.parsed);
    assert_eq!((report.passed, report.failed, report.skipped), (1, 1, 0));
    assert!(report.errors.is_empty(), "{:?}", report.errors);
    assert_eq!(report.failures.len(), 1);
    let failure = &report.failures[0];
    assert_eq!(failure.name, "tests::breaks");
    assert_eq!(at(failure), (Some("src/lib.rs"), Some(14)));
    assert!(
        failure.message.contains("two and two"),
        "{}",
        failure.message
    );
    assert!(!failure.message.contains("RUST_BACKTRACE"));

    // A compile error: the compiler's message at its file and line, and
    // no test list.
    let build = cargo(
        recorded!("cargo_build.stdout"),
        recorded!("cargo_build.stderr"),
        root,
    );
    assert!(build.parsed);
    assert!(build.failures.is_empty());
    assert_eq!((build.passed, build.failed), (0, 0));
    assert_eq!(build.errors.len(), 1, "{:?}", build.errors);
    let error = &build.errors[0];
    assert_eq!(error.name, "error[E0308]");
    assert_eq!(at(error), (Some("src/lib.rs"), Some(9)));
    assert!(
        error.message.contains("mismatched types"),
        "{}",
        error.message
    );

    // The same error in cargo's human format (no JSON asked for).
    let human = cargo(
        "",
        "   Compiling x v0.1.0\nerror[E0425]: cannot find value `c` in this scope\n --> src/lib.rs:2:9\n  |\n2 |     a + c\n  |         ^ not found\n\nerror: could not compile `x` (lib) due to 1 previous error\n",
        root,
    );
    assert_eq!(human.errors.len(), 1, "{:?}", human.errors);
    assert_eq!(at(&human.errors[0]), (Some("src/lib.rs"), Some(2)));

    // A panic in the format before Rust 1.73.
    let old = cargo(
        "test it ... FAILED\n\nfailures:\n\n---- it stdout ----\nthread 'it' panicked at 'boom', tests/x.rs:3:5\n\nfailures:\n    it\n\ntest result: FAILED. 0 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out\n",
        "",
        root,
    );
    assert_eq!(at(&old.failures[0]), (Some("tests/x.rs"), Some(3)));
}

#[test]
fn go_test_json() {
    let root = Some(Path::new("/work/fixture"));
    let report = super::go_test_json(recorded!("go_test.stdout"), "", root);
    assert!(report.parsed);
    assert_eq!((report.passed, report.failed, report.skipped), (1, 1, 1));
    assert_eq!(report.failures.len(), 1, "{:?}", report.failures);
    let failure = &report.failures[0];
    assert_eq!(failure.name, "example.com/fixture.TestBreaks/two");
    assert_eq!(at(failure), (Some("add_test.go"), Some(14)));
    assert!(failure.message.contains("want 5"), "{}", failure.message);
    assert!(!failure.message.contains("=== RUN"));

    let build = super::go_test_json(
        recorded!("go_build.stdout"),
        recorded!("go_build.stderr"),
        root,
    );
    assert!(build.failures.is_empty(), "{:?}", build.failures);
    assert_eq!(build.errors.len(), 1, "{:?}", build.errors);
    assert_eq!(at(&build.errors[0]), (Some("add.go"), Some(3)));
    assert!(build.errors[0].message.contains("undefined: c"));
}

#[test]
fn junit_xml() {
    let pytest = super::junit_xml(recorded!("pytest_junit.xml"), None);
    assert!(pytest.parsed);
    assert_eq!((pytest.passed, pytest.failed, pytest.skipped), (1, 1, 1));
    let failure = &pytest.failures[0];
    assert_eq!(failure.name, "tests.test_add.test_breaks");
    assert_eq!(at(failure), (Some("tests/test_add.py"), Some(9)));
    assert!(
        failure.message.contains("> "),
        "entities resolved: {}",
        failure.message
    );
    assert!(failure.message.contains("assert 4 == 5"));

    let surefire = super::junit_xml(recorded!("surefire.xml"), None);
    assert_eq!((surefire.passed, surefire.failed), (1, 1));
    let error = &surefire.failures[0];
    assert_eq!(error.name, "example.AddTest.breaks");
    assert_eq!(at(error), (Some("Add.java"), Some(7)));
    assert!(error.message.contains("NullPointerException"));
    assert!(
        !error.message.contains("noise"),
        "system-out is not the failure"
    );

    // Not XML at all: nothing parsed, nothing invented.
    let garbage = super::junit_xml("<testsuite><testcase name=", None);
    assert_eq!(garbage.failures, []);
    // An entity the document declares is not expanded.
    let declared = super::junit_xml(
        "<!DOCTYPE x [<!ENTITY e SYSTEM \"file:///etc/passwd\">]><testsuite><testcase name=\"t\"><failure>&e;</failure></testcase></testsuite>",
        None,
    );
    assert_eq!(declared.failed, 1);
    assert_eq!(declared.failures[0].message, "");
}

#[test]
fn vitest_json() {
    let root = Some(Path::new("/work/web"));
    let report = js_json(recorded!("vitest.stdout"), root);
    assert!(report.parsed);
    assert_eq!((report.passed, report.failed, report.skipped), (2, 1, 1));
    assert_eq!(report.failures.len(), 2, "{:?}", report.failures);
    let failure = &report.failures[0];
    assert_eq!(failure.name, "add breaks");
    assert_eq!(at(failure), (Some("src/add.test.ts"), Some(7)));
    assert!(failure.message.contains("expected 4 to be 5"));
    // The file that did not load is a failure of its own.
    let suite = &report.failures[1];
    assert_eq!(at(suite), (Some("src/broken.test.ts"), Some(2)));
    assert!(suite.message.contains("Transform failed"));
}

#[test]
fn jest_json() {
    let root = Some(Path::new("/work/api"));
    let report = js_json(recorded!("jest.stdout"), root);
    assert_eq!((report.passed, report.failed, report.skipped), (1, 1, 0));
    let failure = &report.failures[0];
    assert_eq!(failure.name, "add breaks");
    // No location in the results: the stack frame in the test file.
    assert_eq!(at(failure), (Some("src/add.test.js"), Some(6)));
    assert!(!failure.message.contains('\u{1b}'), "colours removed");
    assert!(failure.message.contains("Expected: 5"));
}

#[test]
fn pytest_summary_without_junit() {
    let report = pytest_text(recorded!("pytest.stdout"), None);
    assert!(report.parsed);
    assert_eq!((report.passed, report.failed, report.skipped), (1, 1, 1));
    assert_eq!(report.failures[0].name, "tests/test_add.py::test_breaks");
    assert_eq!(
        at(&report.failures[0]),
        (Some("tests/test_add.py"), Some(9))
    );
}

#[test]
fn messages_are_bounded_and_unknown_output_is_not_parsed() {
    let long = "é".repeat(MAX_MESSAGE_BYTES);
    let cut = bounded(&long, MAX_MESSAGE_BYTES);
    assert!(cut.len() <= MAX_MESSAGE_BYTES);
    assert!(cut.ends_with('…'));
    let huge = format!(
        "test big ... FAILED\n\n---- big stdout ----\n{}\n\ntest result: FAILED. 0 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out\n",
        "x".repeat(100_000)
    );
    let report = cargo(&huge, "", None);
    assert!(report.failures[0].message.len() <= MAX_MESSAGE_BYTES);
    for unknown in ["hello\n", "{\"not\":\"a report\"}\n"] {
        assert!(!cargo(unknown, "", None).parsed);
        assert!(!super::go_test_json(unknown, "", None).parsed);
        assert!(!js_json(unknown, None).parsed);
        assert!(!pytest_text(unknown, None).parsed);
    }
}

#[test]
fn offline_signatures() {
    assert!(needs_network(
        "error: failed to download from `https://static.crates.io/crates/x/1.0.0/download`"
    ));
    assert!(needs_network(
        "npm error code ENOTFOUND\nnpm error network request to https://registry.npmjs.org failed"
    ));
    assert!(needs_network(
        "go: example.com/x@v1: Get \"https://proxy.golang.org/x/@v/v1.mod\": dial tcp: lookup proxy.golang.org: no such host"
    ));
    assert!(!needs_network("test result: FAILED. 1 passed; 1 failed"));
}
