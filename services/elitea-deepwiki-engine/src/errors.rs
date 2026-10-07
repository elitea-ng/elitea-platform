//! The engine's error contract on the sidecar socket.
//!
//! The Go host reads `{"error": {message, error_type, error_category}}` and
//! maps `error_type` onto its own kinds by the NAME of the Python exception
//! class the old engine raised (`internal/engine.KindOf`). So the wire
//! carries those names, spelled exactly, and `error_category` comes from a
//! port of `elitea_deepwiki.errors.classify` with its precedence intact —
//! including the `[SERVICE_BUSY]` defect ADR-0022 decision 2 freezes.

use std::fmt;

/// The Python exception class a failure is reported as.
///
/// Only the names the host distinguishes, plus `TypeError` (a call with a
/// missing required argument, which the old engine raised from the tool
/// signature) and `Exception` for everything else.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorType {
    FileNotFound,
    Value,
    Memory,
    Key,
    Runtime,
    Type,
    Generic,
}

impl ErrorType {
    /// The type a wire name names: the inverse of [`ErrorType::wire_name`].
    /// Any other name is [`ErrorType::Generic`] (`Exception`).
    #[must_use]
    pub fn from_wire_name(name: &str) -> Self {
        [
            Self::FileNotFound,
            Self::Value,
            Self::Memory,
            Self::Key,
            Self::Runtime,
            Self::Type,
        ]
        .into_iter()
        .find(|kind| kind.wire_name() == name)
        .unwrap_or(Self::Generic)
    }

    /// The class name on the wire.
    #[must_use]
    pub fn wire_name(self) -> &'static str {
        match self {
            Self::FileNotFound => "FileNotFoundError",
            Self::Value => "ValueError",
            Self::Memory => "MemoryError",
            Self::Key => "KeyError",
            Self::Runtime => "RuntimeError",
            Self::Type => "TypeError",
            Self::Generic => "Exception",
        }
    }
}

/// One failed tool run, as the host will read it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineError {
    pub error_type: ErrorType,
    pub message: String,
}

impl EngineError {
    #[must_use]
    pub fn new(error_type: ErrorType, message: impl Into<String>) -> Self {
        Self {
            error_type,
            message: message.into(),
        }
    }

    /// The stop line: the exact error the Python sidecar sent for a
    /// cancelled invocation.
    #[must_use]
    pub fn cancelled() -> Self {
        Self::new(ErrorType::Runtime, "Invocation cancelled")
    }

    /// The message a caller reads. Python's `str(exc) or ClassName`.
    #[must_use]
    pub fn wire_message(&self) -> &str {
        if self.message.is_empty() {
            self.error_type.wire_name()
        } else {
            &self.message
        }
    }

    /// The legacy error category; see [`classify`].
    #[must_use]
    pub fn category(&self) -> &'static str {
        classify(self.error_type, &self.message)
    }

    /// The NDJSON `error` object.
    #[must_use]
    pub fn to_line(&self) -> serde_json::Value {
        serde_json::json!({
            "error": {
                "message": self.wire_message(),
                "error_type": self.error_type.wire_name(),
                "error_category": self.category(),
            }
        })
    }
}

impl fmt::Display for EngineError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.error_type.wire_name(), self.message)
    }
}

impl std::error::Error for EngineError {}

/// The legacy category for a failure. The ORDER of these tests is the
/// contract (`conformance/provider/fixtures/deepwiki/spi/errors.json`,
/// `classifier_precedence`): substring tests run before type tests, so a
/// `ValueError` saying "not found" is `resource_not_found`.
///
/// `RuntimeError` subclasses (`NotImplementedError`, `RecursionError`) are
/// not raised by this engine, so the type test is an equality.
#[must_use]
pub fn classify(error_type: ErrorType, message: &str) -> &'static str {
    let text = message.to_lowercase();
    if text.contains("not found") || error_type == ErrorType::FileNotFound {
        return "resource_not_found";
    }
    if text.contains("[service_busy]") || text.contains("service is busy") {
        return "service_busy";
    }
    if text.contains("download") || text.contains("artifact") {
        return "artifact_error";
    }
    if text.contains("memory") || error_type == ErrorType::Memory {
        return "out_of_memory";
    }
    if text.contains("timeout") {
        return "timeout_error";
    }
    if error_type == ErrorType::Runtime {
        if text.contains("training") {
            return "training_failed";
        }
        if text.contains("inference") || text.contains("generat") {
            return "inference_failed";
        }
        return "runtime_error";
    }
    if error_type == ErrorType::Value {
        return "invalid_input";
    }
    "unknown_error"
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn substring_tests_run_before_type_tests() {
        assert_eq!(
            classify(ErrorType::Value, "Wiki not found"),
            "resource_not_found"
        );
        assert_eq!(classify(ErrorType::Value, "bad input"), "invalid_input");
        assert_eq!(
            classify(ErrorType::Runtime, "LLM generation broke"),
            "inference_failed"
        );
        assert_eq!(classify(ErrorType::Runtime, "boom"), "runtime_error");
        assert_eq!(classify(ErrorType::Key, "boom"), "unknown_error");
        assert_eq!(classify(ErrorType::Memory, "boom"), "out_of_memory");
        assert_eq!(
            classify(ErrorType::FileNotFound, "boom"),
            "resource_not_found"
        );
        assert_eq!(
            classify(ErrorType::Generic, "artifact upload"),
            "artifact_error"
        );
        assert_eq!(
            classify(ErrorType::Generic, "read timeout"),
            "timeout_error"
        );
        assert_eq!(
            classify(ErrorType::Runtime, "training diverged"),
            "training_failed"
        );
    }

    #[test]
    fn a_busy_marker_with_its_own_text_still_classifies_as_busy_here() {
        // The frozen defect lives one hop up: the legacy invoke path STRIPS
        // the marker before classifying. At this hop the marker is intact.
        assert_eq!(
            classify(ErrorType::Runtime, "[SERVICE_BUSY] try later"),
            "service_busy"
        );
    }

    #[test]
    fn wire_names_read_back() {
        for kind in [
            ErrorType::FileNotFound,
            ErrorType::Value,
            ErrorType::Memory,
            ErrorType::Key,
            ErrorType::Runtime,
            ErrorType::Type,
            ErrorType::Generic,
        ] {
            assert_eq!(ErrorType::from_wire_name(kind.wire_name()), kind);
        }
        assert_eq!(ErrorType::from_wire_name("OSError"), ErrorType::Generic);
    }

    #[test]
    fn an_empty_message_reports_the_class_name() {
        let error = EngineError::new(ErrorType::Key, "");
        assert_eq!(error.wire_message(), "KeyError");
    }

    #[test]
    fn the_stop_line_is_the_python_sidecars() {
        assert_eq!(
            EngineError::cancelled().to_line(),
            serde_json::json!({"error": {"message": "Invocation cancelled", "error_type": "RuntimeError", "error_category": "runtime_error"}})
        );
    }
}
