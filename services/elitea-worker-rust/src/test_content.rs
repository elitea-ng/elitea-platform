//! Test helpers for the content controls (ADR-0043 decisions 3 and 4): the
//! model facades put each tool result in an `<untrusted>` envelope and the
//! built-in preamble before each system prompt. Tests that check what the
//! model received read through them.

use elitea_agent_runtime::preamble::BUILTIN_PREAMBLE;

/// The content of a tool message without its `<untrusted>` envelope. Content
/// without an envelope is returned as it is.
pub(crate) fn untrusted_body(content: &str) -> &str {
    content
        .strip_prefix("<untrusted source=\"")
        .and_then(|rest| rest.split_once("\">\n"))
        .and_then(|(_, body)| body.strip_suffix("\n</untrusted>"))
        .unwrap_or(content)
}

/// A system prompt without the built-in preamble. A prompt without it is
/// returned as it is.
pub(crate) fn without_preamble(system: &str) -> &str {
    system
        .strip_prefix(BUILTIN_PREAMBLE)
        .and_then(|rest| rest.strip_prefix("\n\n"))
        .unwrap_or(system)
}
