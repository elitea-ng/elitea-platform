# Prebuilt MCP source mapping

Status: partial and capability-disabled.

This slice admits fixed HTTP definitions from Main's durable catalogue. It does not execute arbitrary local processes.

## Ownership

| Current behavior source | Product behavior | New owner |
| --- | --- | --- |
| SDK `runtime/toolkits/mcp_config.py::McpConfigToolkit` | Resolve `mcp_config` by `server_name`, build dynamic parameter schemas, and filter discovered tools. | Main serves enabled `mcp_*` schemas and resolves their fixed authority. Rust filters and executes tools. |
| SDK `runtime/toolkits/mcp_config.py::_load_http_tools` and `runtime/utils/mcp_oauth.py::substitute_mcp_placeholders` | Replace declared URL and header placeholders before the HTTP connection. | Main `internal/mcpregistry` validates and materializes operator-owned templates after claim. |
| Indexer `methods/indexer_mcp_prebuilt_config.py` | Publish configured server definitions and dynamic `mcp_*` toolkit types. | Main persists definitions in `elitea_mcp.prebuilt_servers` and serves the dynamic toolkit catalogue. |
| Admin plugin configuration and Indexer worker YAML | Let an operator manage platform-wide server definitions. | The new Admin UI edits the Main-owned catalogue. It supports URL templates, templated headers, and dynamic project parameter schemas as YAML mappings. |
| Core `methods/mcp_prebuilt_config.py` | Match normalized server names and toolkit types. | Main `internal/mcpregistry` performs the lookup. |
| Core and SDK configuration expansion | Keep project secret values outside queued execution documents. | Main seals dynamic secret fields on toolkit write and redeems only declared parameters after claim. |

## Runtime contract

Main admits only enabled catalogue entries. Missing and disabled entries fail before worker connection.

Main accepts `mcp_config` selectors and dynamic `mcp_*` types. Both forms resolve through exact normalized catalogue keys.

Main keeps only the selector, declared parameters, tool filters, and cache controls before it signs the command.

The catalogue owns the URL and header templates, timeout, and TLS policy. Caller-supplied connection authority is discarded.

Each `config_schema.properties` entry becomes a field in the dynamic toolkit schema. `secret: true` stays visible to the client and is also projected as JSON Schema `format: password` for Main's project-vault boundary. A toolkit write replaces plaintext with a hidden-secret reference in the same PostgreSQL transaction.

At claim materialization, Main admits only fixed controls and fields declared by that catalogue entry. It redeems those admitted fields, applies defaults and required-field checks, percent-encodes URL path substitutions, rejects placeholders in the URL authority, and rejects queries, fragments, malformed placeholders, control characters, and unsupported types. The final worker document contains the materialized URL and headers, not the source template or parameter map.

The project still owns `selected_tools`, `excluded_tools`, caching controls, and the attached toolkit name.

## Live catalogue updates

PostgreSQL is the authoritative catalogue. Main does not keep a process-local copy.

Admin saves replace one full definition atomically. Later toolkit catalogue reads and execution claims use the new definition across replicas.

Admin withdrawal or deletion removes the definition from later toolkit catalogues. Later execution claims fail closed.

The Admin UI invalidates every active project toolkit catalogue after a successful save or deletion.

An existing execution keeps its signed, claim-materialized input. A catalogue edit never changes authority during that execution.

Rust receives only Main's claim-materialized input. It accepts prebuilt authority from admitted `mcp_config` and `mcp_*` types.

Direct `mcp` retains its existing strict authority path.

Rust bounds header counts and sizes. It marks every configured header value as sensitive. It rejects any remaining template delimiter before connection.

Rust rejects headers that control HTTP framing or MCP protocol negotiation. A claim-fetched OAuth token replaces static `Authorization`.

Delegated authorization retains the original dynamic toolkit type. Exact endpoint and prebuilt aliases can resolve the returned token.

## Verification

Main tests cover schema validation and projection, secret extraction, enabled schema admission, disabled entries, missing rows, dependency failures, exact selector lookup, declared-field admission, authority replacement, URL encoding, and final header materialization.

Claim tests cover forged-marker removal, authoritative connection replacement, redacted failures, and direct-MCP separation.

Rust tests cover static headers, header sensitivity, exclusions, reserved headers, unresolved-template rejection, token precedence, aliases, and authorization identity.

Admin UI tests cover YAML edits, write bodies, invalid mappings, secret preservation, server refusals, and toolkit catalogue invalidation.

PostgreSQL integration tests cover immediate update and deletion visibility across independent service instances.

## Remaining gates

Stdio remains assigned to an external runner with explicit process, package, environment, and egress authority.

Runtime post-discovery authorization failures need typed RMCP call errors before automatic reauthorization can be safe.

Legacy descriptor migration remains a separate deployment capability.

Trusted internal PAT stamping is implemented for configured Main-origin
`/app/{project}/mcp/...` endpoints. Internal builder categories and external
Elitea-as-MCP exposure remain separate platform capabilities; see
`internal-elitea-mcp.md`.
