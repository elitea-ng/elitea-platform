# Shared migration reconciliation: 2026-09-09

## Source and ownership mapping

Main owns product migrations. Rust consumes the resulting runtime and MCP contracts.
The legacy platform supplies behavior references, rather than a database migration design.

| Legacy behavior source | Target contract | Canonical migration |
| --- | --- | --- |
| SDK toolkit execution and Centry indexer toolkit invocation | Main owns bounded read-job inputs, results, and runtime capability constraints. Rust executes `toolkit.execute.read.v1`. | `0125_toolkit_execute_read.sql` |
| Centry indexer prebuilt MCP configuration and project parameters | Main owns the parameter schema. Rust receives resolved MCP configuration through the runtime contract. | `0126_mcp_prebuilt_parameter_schema.sql` |
| Centry MCP OAuth configuration and SDK remote MCP authorization | Main stores encrypted confidential clients. Rust uses delegated authorization without browser-held secrets. | `0127_mcp_oauth_clients.sql` |

The detailed source symbols and implementation boundaries remain in
[external Elitea MCP](external-elitea-mcp.md) and [delegated OAuth DCR](delegated-oauth-dcr.md).
Main retains migrations `0122_webhooks_and_deliveries.sql`, `0123_webhook_delivery_blocked_status.sql`, and `0124_pipeline_runs.sql`.
Their SQL remains unchanged.

## Branch migration history

| Previous branch version | Canonical version | Name | SHA-256 of unchanged SQL bytes |
| --- | --- | --- | --- |
| 122 | 125 | `toolkit_execute_read` | `c2497df59b29fec9230c0164e2e1e44d9c1f5d0d0bf08d7ae1dfd0aea4535e71` |
| 123 | 126 | `mcp_prebuilt_parameter_schema` | `e0d3ebd4b3c7d03810a93d3e824d9a391ff2c870cbd79832c6cdca1028e86289` |
| 124 | 127 | `mcp_oauth_clients` | `62bf7e4c22d2bba3375b216fd578aa1fdb57b1243dada093126d2076b8445515` |

The new files follow the current main head. No SQL content changes during renumbering.
The manifest test expects shared head 127. Integration fixtures use the canonical filenames.
Historical proof sections retain their original version numbers. Those numbers describe the tested database at that time.

## Existing rehearsal database gate

The standard migrator checks each recorded version, name, and checksum before it applies any SQL.
A rehearsal ledger with branch versions 122 through 124 conflicts with the canonical main manifest.
Renaming source files does not reconcile that database.
The earlier 111/112 reconciliation proof does not cover this new collision.

1. Stop rehearsal writers before the actual cutover.
2. Save fresh product and agent-state backups and the complete migration ledger.
3. Restore the product backup into a separately named, isolated rehearsal database.
4. Compare every conflicting receipt with the exact names and checksums above.
5. Stop if a receipt differs or any target version already exists unexpectedly.
6. Verify the existing branch objects, constraints, dependencies, and data against the corresponding SQL.
7. Archive the original receipts with the backup and reconciliation evidence.
8. Prepare a reviewed transaction that moves only the verified branch receipt versions to 125 through 127.
9. Preserve each receipt's name, checksum, and original application metadata.
10. Apply canonical main migrations 122 through 124 through the normal migrator on the isolated copy.
11. Verify shared head 127 and all tenant histories with the unchanged migrator.
12. Compare preserved runtime, MCP, and OAuth data before and after reconciliation.
13. Verify rollback, repeat refusal, and service compatibility on the isolated copy before cutover.

Move only receipts that actually exist. Let the normal migrator apply absent branch migrations.
Do not replace branch checksums with main checksums or mark unapplied main SQL as applied.
Do not delete existing toolkit tables to make the renamed migration run again.
The transaction requires separate implementation and fixture evidence before use.
This source merge performs no database reconciliation and changes no running database.

## Verification boundary

Manifest checks verify unique versions, canonical heads, names, and checksum enforcement.
They do not prove an existing rehearsal database can upgrade or roll back safely.
The separate database reconciliation and deployment checks remain required before runtime cutover.
