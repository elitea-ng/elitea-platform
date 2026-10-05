use adk_rust::{AdkError, ErrorCategory};

use super::{McpErrorRedaction, model_visible_mcp_error};

const ENDPOINT: &str = "https://mcp.internal.example:8443/v1/mcp";
const PAT: &str = "Bearer pat-1234567890abcdef";

fn redaction() -> McpErrorRedaction {
    McpErrorRedaction::new([PAT, "short"], ENDPOINT)
}

fn visible(message: &str) -> String {
    model_visible_mcp_error(
        &AdkError::tool(message.to_owned()),
        "read_wiki",
        &redaction(),
    )
    .message
}

#[test]
fn an_error_result_keeps_the_server_explanation() {
    let error = model_visible_mcp_error(
        &AdkError::tool(
            "MCP tool 'read_wiki' execution failed: Error fetching wiki for X: Repository not found",
        ),
        "read_wiki",
        &redaction(),
    );
    assert_eq!(error.code, "mcp.tool.error_result");
    assert_eq!(error.category, ErrorCategory::Internal);
    assert!(!error.is_retryable());
    assert_eq!(
        error.message,
        "the remote MCP tool returned an error: Error fetching wiki for X: Repository not found"
    );
    assert_eq!(
        visible("MCP tool 'read_wiki' execution failed"),
        "the remote MCP tool returned an error without a description"
    );
}

#[test]
fn every_static_redaction_pattern_compiles() {
    assert_eq!(super::PATTERNS.len(), 4);
}

#[test]
fn a_json_rpc_error_keeps_its_message() {
    assert_eq!(
        visible("Failed to call MCP tool 'read_wiki': Mcp error: -32602: unknown repository"),
        "the remote MCP server rejected the call: -32602: unknown repository"
    );
}

#[test]
fn transport_failures_keep_only_a_fixed_phrase() {
    for (message, expected) in [
        (
            "Failed to call MCP tool 'read_wiki': Transport send error: Client error: error sending request for url (https://mcp.internal.example:8443/v1/mcp)",
            "the remote MCP server could not be reached",
        ),
        (
            "Failed to call MCP tool 'read_wiki': Transport send error: Auth required",
            "the remote MCP server requires authorization (HTTP 401)",
        ),
        (
            "Failed to call MCP tool 'read_wiki': Transport send error: Session expired (HTTP 404)",
            "the remote MCP session expired (HTTP 404); try the call again",
        ),
        (
            "Failed to call MCP tool 'read_wiki': request timeout after 30s",
            "the remote MCP server did not answer in time",
        ),
        (
            "MCP tool 'read_wiki' result is uncertain and was not replayed: Transport closed. Enable tool-call retries only for replay-safe operations",
            "the connection to the remote MCP server was lost; the outcome of the call is unknown",
        ),
        (
            "Failed to call MCP tool 'read_wiki': Transport send error: unexpected server response: HTTP 502 from 10.0.0.7",
            "the remote MCP server returned an unexpected response",
        ),
    ] {
        assert_eq!(visible(message), expected, "{message}");
    }
    // Another tool's message prefix is not this tool's error result.
    assert_eq!(
        visible("MCP tool 'other' execution failed: leaked text"),
        "the remote MCP server could not be reached"
    );
}

#[test]
fn server_text_is_redacted_and_bounded() {
    let message = format!(
        "MCP tool 'read_wiki' execution failed: denied for {PAT} at {ENDPOINT}/repos \
         host mcp.internal.example:8443 \
         url https://alice:hunter22@example.com/x \
         Authorization: Basic YWxpY2U6aHVudGVyMg== \
         api_key=abcd1234efgh token: \"zz-top-secret\" \
         gh ghp_abcdefghijklmnopqrstuvwxyz0123 \
         jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U \
         short\u{7}bell"
    );
    let detail = visible(&message);
    for secret in [
        "pat-1234567890abcdef",
        "mcp.internal.example",
        "hunter22",
        "YWxpY2U6aHVudGVyMg",
        "abcd1234efgh",
        "zz-top-secret",
        "ghp_abcdefghijklmnopqrstuvwxyz0123",
        "eyJhbGciOiJIUzI1NiJ9",
    ] {
        assert!(!detail.contains(secret), "{secret} leaked: {detail}");
    }
    assert!(detail.contains("[redacted]"));
    // A configured value shorter than the floor is not redacted by value.
    assert!(detail.contains("short bell"), "{detail}");
    assert!(!detail.contains('\u{7}'));

    let long = format!(
        "MCP tool 'read_wiki' execution failed: {}",
        "word ".repeat(1_000)
    );
    let detail = visible(&long);
    assert!(detail.ends_with("..."));
    assert!(detail.chars().count() <= "the remote MCP tool returned an error: ".len() + 1_003);
}
