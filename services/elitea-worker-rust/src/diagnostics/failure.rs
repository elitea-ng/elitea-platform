//! Opt-in operator diagnostics. Never format error values or span fields.

use std::backtrace::Backtrace;
use std::fmt::{self, Write as _};
use std::sync::OnceLock;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Instant;

use tracing_subscriber::registry::LookupSpan as _;

use super::DiagnosticInitError;

static ENABLED: AtomicBool = AtomicBool::new(false);
static EPOCH: OnceLock<Instant> = OnceLock::new();
static NEXT_CAPTURE: AtomicU64 = AtomicU64::new(0);
const MAX_BYTES: usize = 8192;
const MAX_SPANS: usize = 32;

pub(super) fn configured(value: Option<&str>) -> Result<bool, DiagnosticInitError> {
    match value.unwrap_or("off").trim() {
        "off" => Ok(false),
        "on" => Ok(true),
        _ => Err(DiagnosticInitError::InvalidFailureDiagnostics),
    }
}

pub(super) fn enable(value: bool) {
    ENABLED.store(value, Ordering::Relaxed);
}

/// Capture once at the runner boundary; emit once through the owning lifecycle.
/// The rate limit is process-wide and uses monotonic time.
pub(crate) fn capture() -> Option<String> {
    if !ENABLED.load(Ordering::Relaxed) {
        return None;
    }
    let seconds = EPOCH.get_or_init(Instant::now).elapsed().as_secs();
    if !admit(&NEXT_CAPTURE, seconds) {
        return None;
    }
    Some(capture_detail())
}

fn admit(next: &AtomicU64, seconds: u64) -> bool {
    next.fetch_update(Ordering::Relaxed, Ordering::Relaxed, |deadline| {
        (seconds >= deadline).then(|| seconds.saturating_add(1))
    })
    .is_ok()
}

fn capture_detail() -> String {
    let mut output = BoundedText(String::with_capacity(MAX_BYTES));
    let _ignored = writeln!(output, "async scope (names only, leaf first):");
    tracing::Span::current().with_subscriber(|(id, dispatch)| {
        let Some(registry) = dispatch.downcast_ref::<tracing_subscriber::Registry>() else {
            return;
        };
        let Some(span) = registry.span(id) else {
            return;
        };
        for span in span.scope().take(MAX_SPANS) {
            let metadata = span.metadata();
            if metadata.target().starts_with("elitea_worker_rust") {
                let _ignored = writeln!(output, "{}", metadata.name());
            }
        }
    });
    let _ignored = writeln!(output, "synchronous stack:\n{}", Backtrace::force_capture());
    output.0
}

struct BoundedText(String);

impl fmt::Write for BoundedText {
    fn write_str(&mut self, text: &str) -> fmt::Result {
        let remaining = MAX_BYTES.saturating_sub(self.0.len());
        if text.len() <= remaining {
            self.0.push_str(text);
            return Ok(());
        }
        let mut end = remaining;
        while !text.is_char_boundary(end) {
            end -= 1;
        }
        self.0.push_str(&text[..end]);
        Err(fmt::Error)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tracing::Instrument as _;

    #[test]
    fn configuration_is_explicit_and_disabled_by_default() {
        assert!(!configured(None).unwrap());
        assert!(configured(Some("on")).unwrap());
        assert!(configured(Some("verbose")).is_err());
    }

    #[test]
    fn rate_limit_admits_one_capture_per_second() {
        let next = AtomicU64::new(0);
        assert!(admit(&next, 0));
        assert!(!admit(&next, 0));
        assert!(admit(&next, 1));
        assert!(!admit(&next, 0));
    }

    #[test]
    fn diagnostic_text_is_utf8_and_bounded() {
        let mut output = BoundedText(String::new());
        assert!(write!(output, "{}", "a".repeat(MAX_BYTES - 1)).is_ok());
        assert!(write!(output, "🙂").is_err());
        assert_eq!(output.0.len(), MAX_BYTES - 1);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn async_ancestry_survives_yield_without_recording_payload_fields() {
        let subscriber = tracing_subscriber::registry();
        let _guard = tracing::subscriber::set_default(subscriber);
        let parent = tracing::info_span!("diagnostic_parent", prompt = "SECRET_PROMPT");
        let output = async {
            let child = tracing::info_span!("diagnostic_child", token = "SECRET_TOKEN");
            async {
                tokio::task::yield_now().await;
                capture_detail()
            }
            .instrument(child)
            .await
        }
        .instrument(parent)
        .await;
        assert!(
            output.contains("diagnostic_child\ndiagnostic_parent"),
            "{output}"
        );
        assert!(output.contains("synchronous stack:"));
        assert!(!output.contains("SECRET_PROMPT"));
        assert!(!output.contains("SECRET_TOKEN"));
        assert!(output.len() <= MAX_BYTES);
    }
}
