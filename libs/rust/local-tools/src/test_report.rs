//! What a test run did, read from the runner's structured output: the
//! counts, and each failing test with its file, line and a short message.
//!
//! | Runner | Read from |
//! |---|---|
//! | cargo | `--message-format=json` compiler messages (`reason: "compiler-message"`, the primary span's file and line), libtest's `test … FAILED` lines and `---- name stdout ----` blocks (`panicked at src/x.rs:12:5`), and its `test result:` lines; human compiler errors (`error[E…]` with ` --> file:line:col`) when the JSON is absent |
//! | go | `go test -json` events (`Action: "fail"`, the test's `Output` lines with `file.go:NN:`); build errors (`file.go:NN:CC: …`) from build output or stderr |
//! | junit XML | `<testcase>` with `<failure>` / `<error>` / `<skipped>` (pytest, Maven Surefire, Gradle) |
//! | vitest, jest | `--reporter=json` / `--json`: `testResults[].assertionResults[]` with `failureMessages` and `location` |
//! | pytest text | `FAILED path::name - message` lines and the summary line, when no junit file was written |
//!
//! Nothing here runs anything, opens a file or expands an entity: the
//! junit reader takes no DTD and resolves only XML's five predefined
//! entities and character references. Every message is bounded
//! ([`MAX_MESSAGE_BYTES`]) and is the runner's text, which the caller
//! returns as tool output (data the model reads, never instructions).

use std::collections::{HashMap, HashSet};
use std::path::Path;

use serde::Serialize;
use serde_json::Value;

/// The longest message one failure carries.
pub const MAX_MESSAGE_BYTES: usize = 2 * 1024;

/// The longest test name one failure carries.
pub const MAX_NAME_BYTES: usize = 512;

/// The most failures (and the most build errors) one report keeps; the
/// counts still count every one.
pub const MAX_KEPT: usize = 200;

/// One failing test, or one build error.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct Failure {
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub file: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub line: Option<u32>,
    /// At most [`MAX_MESSAGE_BYTES`].
    pub message: String,
}

impl Failure {
    fn new(name: &str, file: Option<String>, line: Option<u32>, message: &str) -> Self {
        Self {
            name: bounded(name, MAX_NAME_BYTES),
            file,
            line,
            message: bounded(message, MAX_MESSAGE_BYTES),
        }
    }
}

/// What a run did.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct TestReport {
    pub passed: u64,
    pub failed: u64,
    pub skipped: u64,
    /// The failing tests, in the order the runner reported them.
    pub failures: Vec<Failure>,
    /// Build errors (compiler messages): the tests did not run.
    pub errors: Vec<Failure>,
    /// Whether any structured result was read (a count, a failure or an
    /// error): when not, the caller shows the output's head and tail.
    pub parsed: bool,
}

impl TestReport {
    /// Add `other`'s counts, failures and errors.
    pub fn merge(&mut self, other: Self) {
        self.passed += other.passed;
        self.failed += other.failed;
        self.skipped += other.skipped;
        self.parsed |= other.parsed;
        for failure in other.failures {
            self.push_failure(failure);
        }
        for error in other.errors {
            self.push_error(error);
        }
    }

    fn push_failure(&mut self, failure: Failure) {
        if self.failures.len() < MAX_KEPT {
            self.failures.push(failure);
        }
    }

    fn push_error(&mut self, error: Failure) {
        if self.errors.len() < MAX_KEPT && !self.errors.contains(&error) {
            self.errors.push(error);
        }
    }
}

/// `text` trimmed, ANSI colour codes removed, in at most `max` bytes (cut
/// on a character boundary, ending in `…`).
#[must_use]
pub fn bounded(text: &str, max: usize) -> String {
    let text = strip_ansi(text.trim());
    let text = text.trim();
    if text.len() <= max {
        return text.to_owned();
    }
    let mut end = max.saturating_sub('…'.len_utf8());
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &text[..end])
}

/// `text` without ANSI escape sequences (`ESC [ … letter`).
fn strip_ansi(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '\u{1b}' {
            if chars.peek() == Some(&'[') {
                chars.next();
                for c in chars.by_ref() {
                    if c.is_ascii_alphabetic() {
                        break;
                    }
                }
            }
            continue;
        }
        out.push(c);
    }
    out
}

/// `path:line[:col]` at the start of `text`: the path (made relative to
/// `root` when under it) and the line.
fn location(text: &str, root: Option<&Path>) -> Option<(String, u32)> {
    let text = text.trim();
    let mut parts = text.splitn(3, ':');
    let path = parts.next()?.trim();
    let line: u32 = parts
        .next()?
        .trim()
        .trim_end_matches(|c: char| !c.is_ascii_digit())
        .parse()
        .ok()?;
    if path.is_empty() || path.contains(char::is_whitespace) {
        return None;
    }
    Some((relative(path, root), line))
}

/// `path` relative to `root` when it is under it.
fn relative(path: &str, root: Option<&Path>) -> String {
    let path = path.trim_start_matches("./");
    root.and_then(|root| Path::new(path).strip_prefix(root).ok())
        .map_or_else(|| path.to_owned(), |rest| rest.display().to_string())
}

/// The first `file.ext:NN` in `text` whose extension is a source file's:
/// `tests/test_x.py:12: AssertionError`, `at Foo.bar(FooTest.java:25)`,
/// `at file:///x/src/a.test.ts:4:11`.
fn find_location(text: &str, root: Option<&Path>) -> Option<(String, u32)> {
    const EXTENSIONS: &[&str] = &[
        ".rs", ".go", ".py", ".java", ".kt", ".kts", ".scala", ".groovy", ".ts", ".tsx", ".js",
        ".jsx", ".mjs", ".cjs", ".mts", ".cts", ".vue", ".svelte",
    ];
    for line in text.lines() {
        for (at, _) in line.match_indices(':') {
            let before = &line[..at];
            let Some(ext) = EXTENSIONS.iter().find(|ext| before.ends_with(**ext)) else {
                continue;
            };
            let start = before
                .rfind(|c: char| c.is_whitespace() || "()'\"<>[]".contains(c))
                .map_or(0, |i| i + 1);
            let path = before[start..].trim_start_matches("file://");
            if path.len() <= ext.len() {
                continue;
            }
            let digits: String = line[at + 1..]
                .chars()
                .take_while(char::is_ascii_digit)
                .collect();
            if let Ok(number) = digits.parse::<u32>() {
                return Some((relative(path, root), number));
            }
        }
    }
    None
}

// ── cargo ────────────────────────────────────────────────────────────────

/// A cargo run: `--message-format=json` compiler messages and libtest's
/// human output (both on stdout), human compiler errors on stderr.
#[must_use]
pub fn cargo(stdout: &str, stderr: &str, root: Option<&Path>) -> TestReport {
    let mut report = TestReport::default();
    let mut failed_names: Vec<String> = Vec::new();
    let mut blocks: Vec<(String, String)> = Vec::new();
    let mut block: Option<(String, String)> = None;
    let mut counted_lines = (0, 0, 0);
    let mut result_lines = false;
    for line in stdout.lines() {
        if line.starts_with('{')
            && let Ok(message) = serde_json::from_str::<Value>(line)
        {
            cargo_message(&message, root, &mut report);
            continue;
        }
        if let Some(name) = line
            .strip_prefix("---- ")
            .and_then(|rest| rest.strip_suffix(" stdout ----"))
        {
            if let Some(done) = block.take() {
                blocks.push(done);
            }
            block = Some((name.to_owned(), String::new()));
            continue;
        }
        if let Some((_, text)) = block.as_mut() {
            if line == "failures:" || line.starts_with("test result:") {
                if let Some(done) = block.take() {
                    blocks.push(done);
                }
            } else {
                text.push_str(line);
                text.push('\n');
                continue;
            }
        }
        if let Some(rest) = line.strip_prefix("test ") {
            if let Some(summary) = rest.strip_prefix("result: ") {
                result_lines = true;
                let (passed, failed, ignored) = libtest_counts(summary);
                report.passed += passed;
                report.failed += failed;
                report.skipped += ignored;
            } else if let Some(name) = rest.strip_suffix(" ... FAILED") {
                failed_names.push(name.to_owned());
                counted_lines.1 += 1;
            } else if rest.ends_with(" ... ok") {
                counted_lines.0 += 1;
            } else if rest.contains(" ... ignored") {
                counted_lines.2 += 1;
            }
        }
    }
    if let Some(done) = block.take() {
        blocks.push(done);
    }
    if !result_lines {
        // Cut off before the summary (a timeout): the lines seen so far.
        report.passed += counted_lines.0;
        report.failed += counted_lines.1;
        report.skipped += counted_lines.2;
    }
    for name in &failed_names {
        let text = blocks
            .iter()
            .find(|(block, _)| block == name)
            .map_or("", |(_, text)| text.as_str());
        let (file, line, message) = panic_details(text, root);
        report.push_failure(Failure::new(name, file, line, &message));
    }
    if report.errors.is_empty() {
        for error in human_compiler_errors(stderr, root) {
            report.push_error(error);
        }
    }
    report.parsed |= result_lines || !failed_names.is_empty() || counted_lines != (0, 0, 0);
    report.parsed |= !report.errors.is_empty();
    report
}

/// Whether [`cargo`] reads a line of a `--message-format=json` run's
/// stdout, so the capture keeps it: cargo's artifact, build-script and
/// build-finished records (one per crate built: thousands, kilobytes
/// each) and compiler messages below the error level are left out; error
/// diagnostics and libtest's text are kept. Dropped before the capture's
/// head-and-tail cap, they cannot push the test results out of it.
#[must_use]
pub fn cargo_keeps_line(line: &[u8]) -> bool {
    const DROPPED: &[&[u8]] = &[
        br#"{"reason":"compiler-artifact""#,
        br#"{"reason":"build-script-executed""#,
        br#"{"reason":"build-finished""#,
    ];
    if DROPPED.iter().any(|prefix| line.starts_with(prefix)) {
        return false;
    }
    if line.starts_with(br#"{"reason":"compiler-message""#) {
        // An error is kept; so is a line that does not parse (judged by
        // the reader, not here).
        return serde_json::from_slice::<Value>(line).map_or(true, |message| {
            message
                .pointer("/message/level")
                .and_then(Value::as_str)
                .is_none_or(|level| level.starts_with("error"))
        });
    }
    true
}

/// One `--message-format=json` line: an error's rendered text at its
/// primary span.
fn cargo_message(message: &Value, root: Option<&Path>, report: &mut TestReport) {
    if message.get("reason").and_then(Value::as_str) != Some("compiler-message") {
        return;
    }
    let Some(inner) = message.get("message") else {
        return;
    };
    let level = inner
        .get("level")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if !level.starts_with("error") {
        return;
    }
    let spans = inner.get("spans").and_then(Value::as_array);
    let primary = spans.and_then(|spans| {
        spans
            .iter()
            .find(|span| span.get("is_primary").and_then(Value::as_bool) == Some(true))
            .or_else(|| spans.first())
    });
    let file = primary
        .and_then(|span| span.get("file_name"))
        .and_then(Value::as_str)
        .map(|file| relative(file, root));
    let line = primary
        .and_then(|span| span.get("line_start"))
        .and_then(Value::as_u64)
        .and_then(|line| u32::try_from(line).ok());
    let text = inner
        .get("rendered")
        .and_then(Value::as_str)
        .or_else(|| inner.get("message").and_then(Value::as_str))
        .unwrap_or_default();
    let name = inner
        .get("code")
        .and_then(|code| code.get("code"))
        .and_then(Value::as_str)
        .map_or_else(|| "error".to_owned(), |code| format!("error[{code}]"));
    report.push_error(Failure::new(&name, file, line, text));
    report.parsed = true;
}

/// `ok. 5 passed; 1 failed; 2 ignored; 0 measured; 0 filtered out`.
fn libtest_counts(summary: &str) -> (u64, u64, u64) {
    let mut counts = (0, 0, 0);
    for part in summary.split(['.', ';']) {
        let mut words = part.split_whitespace();
        let (Some(number), Some(what)) = (words.next(), words.next()) else {
            continue;
        };
        let Ok(number) = number.parse::<u64>() else {
            continue;
        };
        match what {
            "passed" => counts.0 += number,
            "failed" => counts.1 += number,
            "ignored" => counts.2 += number,
            _ => {}
        }
    }
    counts
}

/// A failed test's `---- name stdout ----` block: where it panicked and
/// what it said.
fn panic_details(text: &str, root: Option<&Path>) -> (Option<String>, Option<u32>, String) {
    let kept: String = text
        .lines()
        .filter(|line| !line.starts_with("note: run with `RUST_BACKTRACE"))
        .fold(String::new(), |mut out, line| {
            out.push_str(line);
            out.push('\n');
            out
        });
    for line in text.lines() {
        let Some(at) = line.find("panicked at ") else {
            continue;
        };
        let rest = &line[at + "panicked at ".len()..];
        // Before Rust 1.73: `panicked at 'message', src/lib.rs:12:5`.
        let place = if rest.starts_with('\'') {
            rest.rsplit_once("', ").map_or(rest, |(_, place)| place)
        } else {
            rest.trim_end_matches(':')
        };
        if let Some((file, line)) = location(place, root) {
            return (Some(file), Some(line), kept);
        }
    }
    let found = find_location(text, root);
    (
        found.as_ref().map(|(file, _)| file.clone()),
        found.map(|(_, line)| line),
        kept,
    )
}

/// Human compiler errors: `error[E0308]: …` (or `error: …`) blocks with a
/// ` --> file:line:col` line. Cargo's own summaries (`error: could not
/// compile`) have none and are left out.
fn human_compiler_errors(text: &str, root: Option<&Path>) -> Vec<Failure> {
    let mut out = Vec::new();
    let mut current: Option<(String, String)> = None;
    let flush = |current: &mut Option<(String, String)>, out: &mut Vec<Failure>| {
        if let Some((name, block)) = current.take() {
            let place = block
                .lines()
                .find_map(|line| line.trim_start().strip_prefix("--> "))
                .and_then(|place| location(place, root));
            if let Some((file, line)) = place {
                out.push(Failure::new(&name, Some(file), Some(line), &block));
            }
        }
    };
    for line in text.lines() {
        if line.starts_with("error") && !line.starts_with("error: test failed") {
            flush(&mut current, &mut out);
            let name = line.split(':').next().unwrap_or("error").to_owned();
            current = Some((name, format!("{line}\n")));
        } else if line.is_empty() || line.starts_with("warning") {
            flush(&mut current, &mut out);
        } else if let Some((_, block)) = current.as_mut() {
            block.push_str(line);
            block.push('\n');
        }
    }
    flush(&mut current, &mut out);
    out
}

// ── go ───────────────────────────────────────────────────────────────────

/// Whether `line` starts with a Go source file's path and a colon.
fn starts_with_go_file(line: &str) -> bool {
    line.trim().split_once(':').is_some_and(|(path, _)| {
        Path::new(path)
            .extension()
            .is_some_and(|ext| ext.eq_ignore_ascii_case("go"))
    })
}

/// `file.go:NN:CC: message` (a build or vet error line).
fn go_build_error(line: &str, root: Option<&Path>) -> Option<Failure> {
    let trimmed = line.trim();
    if !starts_with_go_file(trimmed) {
        return None;
    }
    let (file, number) = location(trimmed, root)?;
    Some(Failure::new("build", Some(file), Some(number), trimmed))
}

/// One failed Go test: its output without the `=== RUN` and `--- FAIL`
/// lines, at the first `file.go:NN:` it names.
fn go_failure(
    package: &str,
    test: &str,
    outputs: &HashMap<(String, String), String>,
    root: Option<&Path>,
) -> Failure {
    let output = outputs
        .get(&(package.to_owned(), test.to_owned()))
        .map_or("", String::as_str);
    let message: String = output
        .lines()
        .filter(|line| {
            let line = line.trim_start();
            !line.starts_with("=== ") && !line.starts_with("--- ")
        })
        .fold(String::new(), |mut out, line| {
            out.push_str(line);
            out.push('\n');
            out
        });
    let found = message
        .lines()
        .find_map(|line| starts_with_go_file(line).then(|| location(line, root))?);
    let name = if package.is_empty() {
        test.to_owned()
    } else {
        format!("{package}.{test}")
    };
    Failure::new(
        &name,
        found.as_ref().map(|(file, _)| file.clone()),
        found.map(|(_, line)| line),
        &message,
    )
}

/// A `go test -json` run (`stdout`), with build errors on `stderr`.
#[must_use]
pub fn go_test_json(stdout: &str, stderr: &str, root: Option<&Path>) -> TestReport {
    let mut report = TestReport::default();
    // By (package, test): every lookup is one hash, however many tests a
    // run has.
    let mut outputs: HashMap<(String, String), String> = HashMap::new();
    let mut failed: Vec<(String, String)> = Vec::new();
    let mut package_failed: Vec<String> = Vec::new();
    let mut build_lines = String::new();
    for line in stdout.lines() {
        let Ok(event) = serde_json::from_str::<Value>(line) else {
            continue;
        };
        let Some(action) = event.get("Action").and_then(Value::as_str) else {
            continue;
        };
        report.parsed = true;
        let text_of = |name: &str| {
            event
                .get(name)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned()
        };
        let (package, test) = (text_of("Package"), text_of("Test"));
        match (action, test.is_empty()) {
            ("output", _) => {
                let printed = text_of("Output");
                if test.is_empty() {
                    build_lines.push_str(&printed);
                }
                outputs
                    .entry((package, test))
                    .or_default()
                    .push_str(&printed);
            }
            ("build-output", _) => build_lines.push_str(&text_of("Output")),
            ("pass", false) => report.passed += 1,
            ("skip", false) => report.skipped += 1,
            ("fail", false) => failed.push((package, test)),
            ("fail", true) => package_failed.push(package),
            _ => {}
        }
    }
    for text in [build_lines.as_str(), stderr] {
        for line in text.lines() {
            if let Some(error) = go_build_error(line, root) {
                report.push_error(error);
            }
        }
    }
    // A parent whose subtest failed fails too: the leaves are reported.
    // Every failed test's ancestors (`A` and `A/B` of `A/B/C`) are parents.
    let mut parents: HashSet<(&str, &str)> = HashSet::new();
    for (package, test) in &failed {
        for (at, _) in test.match_indices('/') {
            parents.insert((package.as_str(), &test[..at]));
        }
    }
    let failed_packages: HashSet<&str> = failed.iter().map(|(p, _)| p.as_str()).collect();
    let leaves: Vec<&(String, String)> = failed
        .iter()
        .filter(|(package, test)| !parents.contains(&(package.as_str(), test.as_str())))
        .collect();
    report.failed += leaves.len() as u64;
    for (package, test) in leaves {
        report.push_failure(go_failure(package, test, &outputs, root));
    }
    // A package that failed without a failing test (it panicked, or its
    // TestMain did) and without a build error: its own output.
    if report.errors.is_empty() {
        for package in package_failed {
            if failed_packages.contains(package.as_str()) {
                continue;
            }
            let text = outputs
                .get(&(package.clone(), String::new()))
                .map_or("", String::as_str);
            let found = find_location(text, root);
            report.failed += 1;
            report.push_failure(Failure::new(
                &package,
                found.as_ref().map(|(file, _)| file.clone()),
                found.map(|(_, line)| line),
                text,
            ));
        }
    }
    report.parsed |= !report.errors.is_empty();
    report
}

// ── junit XML ────────────────────────────────────────────────────────────

#[derive(Default)]
struct Case {
    name: String,
    classname: String,
    file: Option<String>,
    line: Option<u32>,
    failed: bool,
    skipped: bool,
    message: String,
    text: String,
}

fn case_from(start: &quick_xml::events::BytesStart<'_>) -> Case {
    let mut case = Case::default();
    for attribute in start.attributes().flatten() {
        let value = attribute
            .normalized_value(quick_xml::XmlVersion::Implicit1_0)
            .unwrap_or_default()
            .into_owned();
        match attribute.key.as_ref() {
            "name" => case.name = value,
            "classname" => case.classname = value,
            "file" => case.file = Some(value),
            "line" => case.line = value.parse().ok(),
            _ => {}
        }
    }
    case
}

fn message_attribute(start: &quick_xml::events::BytesStart<'_>) -> String {
    start
        .attributes()
        .flatten()
        .find(|attribute| attribute.key.as_ref() == "message")
        .and_then(|attribute| {
            attribute
                .normalized_value(quick_xml::XmlVersion::Implicit1_0)
                .ok()
        })
        .map(std::borrow::Cow::into_owned)
        .unwrap_or_default()
}

/// `&lt;` and friends, and character references, as text.
fn entity(name: &str) -> Option<char> {
    if let Some(number) = name.strip_prefix('#') {
        let value = match number.strip_prefix('x') {
            Some(hex) => u32::from_str_radix(hex, 16).ok()?,
            None => number.parse().ok()?,
        };
        return char::from_u32(value).filter(|c| *c != '\0');
    }
    match name {
        "lt" => Some('<'),
        "gt" => Some('>'),
        "amp" => Some('&'),
        "quot" => Some('"'),
        "apos" => Some('\''),
        _ => None,
    }
}

impl Case {
    /// A `<failure>`, `<error>` or `<skipped>` child; whether its text is
    /// the failure's (an opening failure or error tag).
    fn child(&mut self, start: &quick_xml::events::BytesStart<'_>, opens: bool) -> bool {
        match start.local_name().as_ref() {
            "failure" | "error" => {
                self.failed = true;
                self.message = message_attribute(start);
                opens
            }
            "skipped" => {
                self.skipped = true;
                false
            }
            _ => false,
        }
    }

    /// Count the case, and keep it when it failed.
    fn finish(self, report: &mut TestReport, root: Option<&Path>) {
        report.parsed = true;
        if !self.failed {
            if self.skipped {
                report.skipped += 1;
            } else {
                report.passed += 1;
            }
            return;
        }
        report.failed += 1;
        let message = if self.text.trim().is_empty() {
            self.message.clone()
        } else if self.message.is_empty() || self.text.contains(&self.message) {
            self.text.clone()
        } else {
            format!("{}\n{}", self.message, self.text)
        };
        let (file, line) = match (self.file, self.line) {
            (Some(file), Some(line)) => (Some(relative(&file, root)), Some(line)),
            (file, _) => match find_location(&self.text, root) {
                Some((found, line)) => (Some(found), Some(line)),
                None => (file.map(|file| relative(&file, root)), None),
            },
        };
        let name = if self.classname.is_empty() {
            self.name
        } else {
            format!("{}.{}", self.classname, self.name)
        };
        report.push_failure(Failure::new(&name, file, line, &message));
    }
}

/// junit XML (pytest's `--junitxml`, Surefire's and Gradle's reports).
/// A document that does not parse keeps what was read before the error.
#[must_use]
pub fn junit_xml(xml: &str, root: Option<&Path>) -> TestReport {
    use quick_xml::events::Event;
    let mut report = TestReport::default();
    let mut reader = quick_xml::Reader::from_str(xml);
    let mut case: Option<Case> = None;
    let mut in_failure = false;
    loop {
        let event = match reader.read_event() {
            Ok(Event::Eof) | Err(_) => break,
            Ok(event) => event,
        };
        match event {
            Event::Start(start) | Event::Empty(start)
                if start.local_name().as_ref() == "testcase" =>
            {
                if let Some(open) = case.take() {
                    open.finish(&mut report, root);
                }
                case = Some(case_from(&start));
            }
            Event::Start(start) => {
                if let Some(case) = case.as_mut() {
                    in_failure = case.child(&start, true);
                }
            }
            Event::Empty(start) => {
                if let Some(case) = case.as_mut() {
                    case.child(&start, false);
                }
            }
            Event::Text(text) if in_failure => {
                if let Some(case) = case.as_mut() {
                    case.text.push_str(&text.xml10_content());
                }
            }
            Event::CData(text) if in_failure => {
                if let Some(case) = case.as_mut() {
                    case.text.push_str(&text.xml10_content());
                }
            }
            Event::GeneralRef(reference) if in_failure => {
                if let (Some(case), Some(c)) = (case.as_mut(), entity(&reference)) {
                    case.text.push(c);
                }
            }
            Event::End(end) => match end.local_name().as_ref() {
                "failure" | "error" => in_failure = false,
                "testcase" => {
                    if let Some(open) = case.take() {
                        open.finish(&mut report, root);
                    }
                }
                _ => {}
            },
            _ => {}
        }
    }
    if let Some(open) = case.take() {
        open.finish(&mut report, root);
    }
    report
}

// ── vitest, jest ─────────────────────────────────────────────────────────

/// The JSON document in `stdout`: all of it, or the first line that starts
/// one (a reporter's JSON after a build's chatter).
fn json_document(stdout: &str) -> Option<Value> {
    if let Ok(value) = serde_json::from_str::<Value>(stdout.trim()) {
        return Some(value);
    }
    let mut offset = 0;
    for line in stdout.split_inclusive('\n') {
        if line.starts_with('{') {
            let mut values =
                serde_json::Deserializer::from_str(&stdout[offset..]).into_iter::<Value>();
            if let Some(Ok(value)) = values.next()
                && value.get("testResults").is_some()
            {
                return Some(value);
            }
        }
        offset += line.len();
    }
    None
}

/// A vitest (`--reporter=json`) or jest (`--json`) report on stdout.
#[must_use]
pub fn js_json(stdout: &str, root: Option<&Path>) -> TestReport {
    let mut report = TestReport::default();
    let Some(document) = json_document(stdout) else {
        return report;
    };
    let Some(results) = document.get("testResults").and_then(Value::as_array) else {
        return report;
    };
    report.parsed = true;
    let count = |name: &str| document.get(name).and_then(Value::as_u64).unwrap_or(0);
    report.passed = count("numPassedTests");
    report.failed = count("numFailedTests");
    report.skipped = count("numPendingTests") + count("numTodoTests");
    for suite in results {
        let path = suite
            .get("name")
            .and_then(Value::as_str)
            .map(|path| relative(path, root));
        let assertions = suite
            .get("assertionResults")
            .and_then(Value::as_array)
            .map(Vec::as_slice)
            .unwrap_or_default();
        let mut any_failed = false;
        for assertion in assertions {
            if assertion.get("status").and_then(Value::as_str) != Some("failed") {
                continue;
            }
            any_failed = true;
            let name = assertion
                .get("fullName")
                .and_then(Value::as_str)
                .or_else(|| assertion.get("title").and_then(Value::as_str))
                .unwrap_or("test");
            let message = assertion
                .get("failureMessages")
                .and_then(Value::as_array)
                .map(|messages| {
                    messages
                        .iter()
                        .filter_map(Value::as_str)
                        .collect::<Vec<_>>()
                        .join("\n")
                })
                .unwrap_or_default();
            let line = assertion
                .get("location")
                .and_then(|location| location.get("line"))
                .and_then(Value::as_u64)
                .and_then(|line| u32::try_from(line).ok())
                .or_else(|| {
                    // jest without --testLocationInResults: the stack frame
                    // in the test file.
                    let file = path.as_deref()?;
                    let base = Path::new(file).file_name()?.to_str()?;
                    message.lines().find_map(|line| {
                        let (found, number) = find_location(line, root)?;
                        found.ends_with(base).then_some(number)
                    })
                });
            report.push_failure(Failure::new(name, path.clone(), line, &message));
        }
        // A file that failed to load or run (a syntax error, a failing
        // hook): no assertion failed, the suite did.
        if !any_failed && suite.get("status").and_then(Value::as_str) == Some("failed") {
            let message = suite
                .get("message")
                .and_then(Value::as_str)
                .unwrap_or("the test file failed");
            let found = find_location(message, root);
            report.failed = report.failed.max(1);
            report.push_failure(Failure::new(
                path.as_deref().unwrap_or("test file"),
                path.clone(),
                found.map(|(_, line)| line),
                message,
            ));
        }
    }
    report
}

// ── pytest text ──────────────────────────────────────────────────────────

/// pytest's own summary when no junit file was read: `FAILED path::name -
/// message` lines and `1 failed, 2 passed in 0.1s`.
#[must_use]
pub fn pytest_text(stdout: &str, root: Option<&Path>) -> TestReport {
    let mut report = TestReport::default();
    for line in stdout.lines() {
        let line = line.trim();
        if let Some(rest) = line
            .strip_prefix("FAILED ")
            .or_else(|| line.strip_prefix("ERROR "))
        {
            let (id, message) = rest.split_once(" - ").unwrap_or((rest, ""));
            let file = id.split("::").next().map(|file| relative(file, root));
            let number = file.as_ref().and_then(|file| {
                stdout.lines().find_map(|line| {
                    let rest = line.trim().strip_prefix(file.as_str())?.strip_prefix(':')?;
                    rest.split(':').next()?.parse().ok()
                })
            });
            report.push_failure(Failure::new(id, file, number, message));
            report.parsed = true;
        }
        let summary = line.trim_matches('=').trim();
        if summary.contains(" in ")
            && (summary.contains("passed") || summary.contains("failed"))
            && summary.chars().next().is_some_and(|c| c.is_ascii_digit())
        {
            report.parsed = true;
            let counts = summary.split(" in ").next().unwrap_or_default();
            for part in counts.split(',') {
                let mut words = part.split_whitespace();
                let (Some(number), Some(what)) = (words.next(), words.next()) else {
                    continue;
                };
                let Ok(number) = number.parse::<u64>() else {
                    continue;
                };
                match what {
                    "passed" => report.passed = number,
                    "failed" => report.failed = number,
                    "skipped" | "xfailed" | "deselected" => report.skipped += number,
                    "error" | "errors" => report.failed += number,
                    _ => {}
                }
            }
        }
    }
    report
}

// ── the network ──────────────────────────────────────────────────────────

/// Whether a failed offline run looks like it needed the network to fetch
/// dependencies (cargo `failed to download`, npm `ENOTFOUND`, go `dial
/// tcp`, Maven and Gradle unable to resolve artifacts).
#[must_use]
pub fn needs_network(output: &str) -> bool {
    const SIGNATURES: &[&str] = &[
        // cargo
        "failed to download",
        "failed to get `",
        "Unable to update registry",
        "failed to query replaced source registry",
        "Couldn't resolve host",
        "failed to load source for dependency",
        // npm, npx, pnpm, yarn
        "ENOTFOUND",
        "EAI_AGAIN",
        "getaddrinfo",
        "ERR_PNPM_META_FETCH_FAIL",
        "npm error network",
        "npm ERR! network",
        // go
        "dial tcp",
        "proxy.golang.org",
        // pip, maven, gradle
        "Could not transfer artifact",
        "Could not resolve dependencies",
        "UnknownHostException",
        "Temporary failure in name resolution",
        "nodename nor servname provided",
    ];
    SIGNATURES
        .iter()
        .any(|signature| output.contains(signature))
}

#[cfg(test)]
mod tests;
