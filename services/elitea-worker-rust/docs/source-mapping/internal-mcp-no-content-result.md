# Internal MCP no-content results, 2026-09-14

## Failure and ownership

The deployed MCP chat acceptance finds a false failure after participant removal.
The shared Main handler successfully removes the mapping and returns HTTP 204 with an empty body.
The MCP response adapter checks JSON validity before its empty-body fallback.
An empty body fails that check, so the client receives an error after a successful mutation.
This can cause an unnecessary retry of an operation that already completes.

Current Core `api/v2/participant.py::PromptLibAPI.delete` publishes participant removal through MCP.
It delegates to `utils/participant_utils.py::delete_participant_from_conversation`.
The preserved behavior removes the conversation mapping and refuses removal of its author.
The current platform supplies the business contract; its transport implementation is not copied.

Main `internal/api/v2/conversations/handler.go::RemoveParticipant` owns the shared HTTP response.
Main `internal/infra/db/repos/conversations.go::RemoveParticipant` owns the mapping transaction and author check.
Main `internal/api/v2/mcp/server.go::callInternalTool` owns the MCP result conversion.
Rust `src/toolkits/mcp.rs` invokes this existing endpoint and consumes its result.
No Rust protocol, checkpoint, or database schema change is required.

## Correction

The adapter converts an empty HTTP 204 response into the existing empty JSON object result.
It continues to reject malformed JSON, empty HTTP 200 responses, and server errors.
Safe validation failures retain their error result.
The adapter invokes the operation once and does not retry it.

## Verification

The regression first fails for empty and whitespace-only HTTP 204 responses.
After the correction, 45 focused MCP checks pass with no skips.
The expanded internal MCP suite then passes 254 checks against disposable databases, with no skips.
The eight existing `TestChatAuthority` PostgreSQL tests also pass with no skips.
They exercise disposable databases, including actor ownership, private resources, folder mutation, participant removal, and rollback.
`go vet ./internal/api/v2/mcp` passes.

Before deployment, an independent PAT client receives an error for removal in temporary chat 585.
Its next conversation read proves that the participant mapping is absent.
This confirms a response-conversion defect rather than a failed database deletion.

After deployment, a fresh headed Chrome session creates a temporary PAT.
An independent client exercises all eleven ordinary chat operations through the deployed MCP endpoint.
Seventeen checks cover creation, reads, updates, folders, participant settings, removal, author-removal refusal, and forged-author/project refusal.
Temporary chat 586 retains its updated instructions and has no removed application participant.
The browser displays the renamed chat inside its renamed folder before and after reload.
Chat and folder deletion return HTTP 204. PAT revocation returns HTTP 204.
The complete script exits with status zero.

Evidence: `elitea-chat-delete-repro.log`, `elitea-chat-positive-browser-verified.log`, and `elitea-chat-positive-browser.png`.
Main image: `sha256:17984c30c0d44c3dd731cfa8694bbd433eacc10a9c0c3561dd69b9304c5acf6d`.
The deployment preserves all six Main mounts, environment values, networks, resource limits, and existing database selection.
Rust and Web retain their previous images.

## Permission boundary follow-up

The deployed checks use an existing account's permissions.
They do not prove operation denial for a live project member whose chat grants differ.
The existing protocol tests verify each exact permission; PostgreSQL tests separately verify object authority.
A proposed live test requires a temporary role for the dedicated test user in project 2.
Automatic approval review rejects that access change without explicit authorization for its recipient, project, and permissions.
No live role or membership changes execute in that attempt.
That attempt leaves the restricted live operation matrix open.
The narrowed proposal grants only `projects.projects.project.view` and `models.chat.conversations.list` to test user 6 in project 2.
It creates a temporary named role, verifies the other operation permissions are refused, then removes the role, membership, and PAT.
The proposal does not grant viewer, editor, or administrator roles.

The user subsequently approves this exact temporary access through the chat guardrail.
The [restricted acceptance](internal-mcp-restricted-chat-acceptance.md) then passes with successful project admission, eleven exact permission refusals, and complete cleanup.
This closes the remaining point 3 operation-permission check.
