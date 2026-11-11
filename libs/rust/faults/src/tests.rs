use std::collections::HashSet;
use std::error::Error;
use std::fmt;
use std::time::{Duration, UNIX_EPOCH};

use tracing_subscriber::layer::SubscriberExt as _;
use tracing_subscriber::util::SubscriberInitExt as _;

use super::*;
use crate::operator_stream::rfc3339_utc;

const SECRET: &str = "password=hunter2-SECRET-CANARY";

#[derive(Debug)]
struct SecretError(Option<Box<dyn Error + Send + Sync + 'static>>);

impl fmt::Display for SecretError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "connect failed: {SECRET}")
    }
}

impl Error for SecretError {
    fn source(&self) -> Option<&(dyn Error + 'static)> {
        self.0
            .as_deref()
            .map(|error| error as &(dyn Error + 'static))
    }
}

#[derive(Debug)]
struct TestFault(Option<Box<dyn Error + 'static>>);

impl fmt::Display for TestFault {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "fault with {SECRET}")
    }
}

impl Error for TestFault {
    fn source(&self) -> Option<&(dyn Error + 'static)> {
        self.0.as_deref()
    }
}

impl Fault for TestFault {
    fn fault_code(&self) -> Code {
        codes::INTERNAL_UNEXPECTED
    }

    fn fault_retryable(&self) -> Option<bool> {
        Some(false)
    }
}

#[test]
fn registry_is_well_formed_and_codes_are_unique() {
    assert!(REGISTRY.len() >= 2);
    let mut seen = HashSet::new();
    for entry in REGISTRY {
        assert!(seen.insert(entry.code()), "duplicate {}", entry.code());
        let segments: Vec<&str> = entry.code().split('.').collect();
        assert!(segments.len() >= 2, "{}", entry.code());
        for segment in segments {
            assert!(
                segment.starts_with(|c: char| c.is_ascii_lowercase())
                    && segment
                        .chars()
                        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_'),
                "{}",
                entry.code()
            );
        }
        for text in [entry.sentence(), entry.action()] {
            assert!(!text.is_empty() && text.len() <= 256, "{}", entry.code());
            assert!(
                !text.contains(['{', '}', '%', '/', '\\']) && !text.contains("://"),
                "{}",
                entry.code()
            );
        }
        assert!(entry.sentence().ends_with('.'), "{}", entry.code());
        assert_ne!(entry.owner(), "");
        assert_eq!(
            Code::lookup(entry.code()).map(Code::as_str),
            Some(entry.code())
        );
    }
    assert_eq!(codes::INTERNAL_PANIC.as_str(), "internal.panic");
    assert_eq!(
        codes::INTERNAL_PANIC.entry().retryable(),
        Retryable::NotApplicable
    );
    assert!(Code::lookup("internal.no_such_code").is_none());
}

#[test]
fn chain_is_bounded_in_length_and_bytes() {
    let mut error: Box<dyn Error + Send + Sync> = Box::new(SecretError(None));
    for _ in 0..20 {
        error = Box::new(SecretError(Some(error)));
    }
    let chain = cause_chain(error.as_ref(), &[render_type_name::<SecretError>]);
    assert_eq!(chain.len(), MAX_CAUSES);
    let long = bounded("é".repeat(MAX_CAUSE_BYTES));
    assert!(long.len() <= MAX_CAUSE_BYTES);
    assert!(long.chars().all(|c| c == 'é'));
}

#[test]
fn unknown_types_render_as_type_name_or_placeholder_never_as_text() {
    let error = SecretError(Some(Box::new(SecretError(None))));
    let unregistered = cause_chain(&error, DEFAULT_RENDERERS);
    assert_eq!(unregistered, vec![UNREGISTERED_CAUSE, UNREGISTERED_CAUSE]);
    let named = cause_chain(&error, &[render_type_name::<SecretError>]);
    assert_eq!(named.len(), 2);
    for element in unregistered.iter().chain(&named) {
        assert!(!element.contains("hunter2"), "{element}");
        assert!(!element.contains("connect failed"), "{element}");
    }
    assert!(named[0].ends_with("SecretError"), "{named:?}");
}

#[test]
fn io_error_renders_its_kind_only() {
    let error = std::io::Error::new(std::io::ErrorKind::NotFound, "/srv/private/hunter2.key");
    assert_eq!(cause_chain(&error, DEFAULT_RENDERERS), vec!["io: NotFound"]);
}

#[test]
fn fault_renders_code_and_sentence() {
    let error = TestFault(None);
    let chain = cause_chain(&error, &[render_fault::<TestFault>]);
    assert_eq!(
        chain,
        vec![format!(
            "internal.unexpected: {}",
            codes::INTERNAL_UNEXPECTED.sentence()
        )]
    );
}

#[test]
fn rfc3339_is_utc_with_microseconds() {
    assert_eq!(rfc3339_utc(UNIX_EPOCH), "1970-01-01T00:00:00.000000Z");
    let time = UNIX_EPOCH + Duration::new(1_700_000_000, 123_456_000);
    assert_eq!(rfc3339_utc(time), "2023-11-14T22:13:20.123456Z");
    let leap = UNIX_EPOCH + Duration::from_hours(264_384);
    assert_eq!(rfc3339_utc(leap), "2000-02-29T00:00:00.000000Z");
}

const STREAM_CHILD: &str = "ELITEA_FAULTS_STREAM_CHILD";

#[test]
fn operator_stream_is_one_json_object_per_event_with_span_fields() {
    let output = logtest::run_child("tests::operator_stream_child", STREAM_CHILD);
    let lines = output.operator_lines();
    assert_eq!(lines.len(), 3, "{}", output.stdout);
    assert!(!output.stdout.contains("time.busy"), "{}", output.stdout);
    assert!(!output.stdout.contains(SECRET), "{}", output.stdout);

    let plain = &lines[0];
    assert_eq!(plain["msg"], "outside any span");
    assert_eq!(plain["level"], "INFO");
    assert!(plain.get("span").is_none());
    let time = plain["time"].as_str().unwrap();
    assert!(time.ends_with('Z') && time.len() == 27, "{time}");

    let nested = &lines[1];
    assert_eq!(nested["msg"], "inside");
    assert_eq!(nested["target"], "elitea_faults::tests");
    assert_eq!(nested["span"], "inner");
    assert_eq!(nested["execution_id"], "exec-1");
    assert_eq!(nested["support_ref"], "exec-1");
    assert_eq!(nested["generation"], 3);
    assert_eq!(nested["shared"], "inner", "an inner span's field wins");
    assert_eq!(nested["recorded"], true, "a recorded span field is kept");
    assert_eq!(nested["event_field"], "event", "an event field wins");
    let keys: Vec<&String> = nested.keys().collect();
    assert!(keys.len() > 4);

    let fault = &lines[2];
    assert_eq!(fault["level"], "WARN");
    assert_eq!(fault["msg"], codes::INTERNAL_UNEXPECTED.sentence());
    assert_eq!(fault["code"], "internal.unexpected");
    assert_eq!(fault["action"], codes::INTERNAL_UNEXPECTED.action());
    assert_eq!(fault["retryable"], false);
    assert_eq!(fault["event"], "test_fault");
    assert_eq!(fault["support_ref"], "exec-1");
    assert_eq!(
        fault["cause"],
        serde_json::json!(["io: PermissionDenied", UNREGISTERED_CAUSE])
    );
}

#[test]
fn operator_stream_child() {
    if !logtest::is_child(STREAM_CHILD) {
        return;
    }
    tracing_subscriber::registry()
        .with(OperatorJsonLayer::new(std::io::stdout))
        .init();
    tracing::info!("outside any span");
    let outer = tracing::info_span!(
        "outer",
        execution_id = "exec-1",
        support_ref = "exec-1",
        shared = "outer",
        event_field = "outer"
    );
    let _outer = outer.enter();
    let inner = tracing::info_span!(
        "inner",
        generation = 3_u64,
        shared = "inner",
        recorded = tracing::field::Empty
    );
    inner.record("recorded", true);
    {
        let _inner = inner.enter();
        tracing::info!(event_field = "event", "inside");
    }
    drop(inner);
    // io::Error's source() is its inner error's source: the inner SecretError
    // is skipped, its own source renders as unregistered.
    let error = TestFault(Some(Box::new(std::io::Error::new(
        std::io::ErrorKind::PermissionDenied,
        SecretError(Some(Box::new(SecretError(None)))),
    ))));
    fault!(tracing::Level::WARN, &error, event = "test_fault");
    logtest::child_ran();
}

/// A wrapper that only repeats its inner error keeps its own label; nothing
/// left means the element is dropped; equal or unrelated text is untouched.
#[test]
fn a_wrapper_repeating_its_inner_text_keeps_only_its_own_part() {
    assert_eq!(
        without_inner("output: frame refused", "output session: frame refused"),
        "output"
    );
    assert_eq!(without_inner("spool: disk: full", "disk: full"), "spool");
    assert_eq!(
        without_inner("frame refused", "frame refused"),
        "frame refused"
    );
    assert_eq!(without_inner(": frame refused", "frame refused"), "");
    assert_eq!(
        without_inner("spool: write failed", "io: NotFound"),
        "spool: write failed"
    );
    assert_eq!(without_inner("abc", ""), "abc");

    let error = Wrapper(Wrapper(Leaf));
    let chain = cause_chain(
        &error,
        &[
            render_safe::<Wrapper<Wrapper<Leaf>>>,
            render_safe::<Wrapper<Leaf>>,
            render_safe::<Leaf>,
        ],
    );
    assert_eq!(chain, vec!["outer", "inner", "leaf: frame refused"]);
    let repeat = Wrapper(Leaf);
    let chain = cause_chain(&repeat, &[render_safe::<Leaf>]);
    assert_eq!(chain, vec![UNREGISTERED_CAUSE, "leaf: frame refused"]);
    // A label-less wrapper that is all repeat disappears from the chain.
    let bare = Wrapper(Leaf);
    let chain = cause_chain(&bare, &[render_bare_wrapper, render_safe::<Leaf>]);
    assert_eq!(chain, vec!["leaf: frame refused"]);
}

#[test]
fn the_operator_stream_mark_is_cleared_by_unwinding() {
    assert!(!in_operator_stream());
    let caught = std::panic::catch_unwind(|| {
        let _mark = OperatorStreamMark::enter();
        assert!(in_operator_stream());
        let nested = OperatorStreamMark::enter();
        drop(nested);
        assert!(in_operator_stream());
        panic!("unwind through the mark");
    });
    assert!(caught.is_err());
    assert!(!in_operator_stream());
}

#[derive(Debug)]
struct Leaf;

impl fmt::Display for Leaf {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("frame refused")
    }
}

impl Error for Leaf {}

impl SafeCause for Leaf {
    fn safe_cause(&self) -> String {
        format!("leaf: {self}")
    }
}

/// Displays as its inner error, like `OutputGrpcError::Protocol`.
#[derive(Debug)]
struct Wrapper<E>(E);

impl<E: fmt::Display> fmt::Display for Wrapper<E> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

impl<E: Error + 'static> Error for Wrapper<E> {
    fn source(&self) -> Option<&(dyn Error + 'static)> {
        Some(&self.0)
    }
}

impl SafeCause for Wrapper<Leaf> {
    fn safe_cause(&self) -> String {
        format!("inner: {self}")
    }
}

impl SafeCause for Wrapper<Wrapper<Leaf>> {
    fn safe_cause(&self) -> String {
        format!("outer: {self}")
    }
}

/// Renders a `Wrapper<Leaf>` as `: <inner text>`, nothing of its own.
fn render_bare_wrapper(error: &(dyn Error + 'static)) -> Option<String> {
    error
        .downcast_ref::<Wrapper<Leaf>>()
        .map(|wrapper| format!(": {wrapper}"))
}
