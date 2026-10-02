# Restricted internal MCP chat acceptance

Date: 2026-09-14. Result: pass, with complete cleanup and no skipped cases.
This check closes the final point 3 progression requirement.
Production activation, wider recovery, and the graph and sensitive-tool gates remain separate.

## Scope and authorization

The user explicitly approves temporary access for the dedicated restricted test user in project 2.
The named role grants only `projects.projects.project.view` and `models.chat.conversations.list`.
The script first verifies the user's exact identity and the absence of both membership and role.
It inserts the role, two grants, and membership in one transaction.
It adds no administrator, editor, or viewer role and changes no schema.

A fresh headed Chrome session signs in as that user and creates a ten-minute PAT through Personal Tokens.
The token stays in memory. An independent Python HTTP client calls `/app/2/mcp/elitea_core/chat` with Bearer authentication.
The browser check covers token creation and revocation; the operation matrix is an external protocol check, not a browser interaction claim.

## Source ownership

The current Core conversation, participant, folder, and message routes define the business behavior mapped in [chat authority](internal-mcp-chat-authority.md).
Main `internal/api/v2/mcp/internal_chat_catalog.go` publishes the operation descriptors and required permissions.
Main `internal/api/v2/mcp/server.go` admits the project, checks operation permission, and invokes the shared HTTP handlers.
Those handlers and repositories retain product visibility and mutation ownership.
Rust `src/toolkits/mcp.rs` consumes the same MCP endpoint during chat execution.
This acceptance changes no runtime code and adds no MCP-specific execution or persistence path.

## Observed matrix

Every call returns HTTP 200 at the MCP transport layer.
Listing succeeds without `isError`; every denied operation returns `isError: true`, the exact permission below, and a statement that nothing executes.
Thus the test distinguishes operation refusal from project admission failure.

| Tool | Required permission | Result |
| --- | --- | --- |
| `get_elitea_core_conversations` | `models.chat.conversations.list` | Success |
| `post_elitea_core_conversations` | `models.chat.conversations.create` | Refused |
| `get_elitea_core_conversation` | `models.chat.conversation.details` | Refused |
| `put_elitea_core_conversation` | `models.chat.conversation.update` | Refused |
| `get_elitea_core_participant` | `models.chat.participant.get` | Refused |
| `delete_elitea_core_participant` | `models.chat.participant.delete` | Refused |
| `post_elitea_core_participants` | `models.chat.participants.create` | Refused |
| `patch_elitea_core_entity_settings` | `models.chat.entity_settings.update` | Refused |
| `get_elitea_core_folder` | `models.chat.folders.get` | Refused |
| `post_elitea_core_folder` | `models.chat.folders.create` | Refused |
| `put_elitea_core_folder` | `models.chat.folders.update` | Refused |
| `post_elitea_core_messages` | `models.chat.messages.create` | Refused |

Negative calls use nonexistent target IDs where a target is required.
Their assertions require the operation-permission response, not a missing-object error.
Object-level authorization has separate PostgreSQL evidence in the authority ledger.
The [positive deployed matrix](internal-mcp-no-content-result.md) covers the ordinary operations and persisted results.

## Cleanup and evidence

PAT revocation returns HTTP 204.
Cleanup removes the exact temporary membership, the named role's permission rows, and the named role in one transaction.
Final membership and named-role counts are zero, matching the initial baseline.
The complete script exits with status zero.

Evidence: `elitea-chat-readonly-role-acceptance.py` and `elitea-chat-readonly-role-acceptance.log`.
Main image: `sha256:17984c30c0d44c3dd731cfa8694bbd433eacc10a9c0c3561dd69b9304c5acf6d`.
No deployment, database replacement, or schema migration occurs during this acceptance.
