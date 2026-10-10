# Toolkit port batch b7: data and runtime types (2026-10-10)

Batch b7 of the SDK toolkit port (ADR-0027) covered `bigquery`, `delta_lake`,
`aws`, `memory`, `vectorstore`, `sandbox` and `mcp_config`, read against the
worker-pinned SDK `b5113a1` (`services/elitea-worker-python/elitea-sdk.lock.json`).

`bigquery` is ported as a partial family (8 of 11 tools; see
`configuration-toolsets.md`, "BigQuery partial warehouse family"), and
`mcp_config` is declared in the Rust worker capability because the native MCP
branch already serves it (`SOURCE_PARITY.md`). The types below were NOT ported.
Each section records what the SDK actually does, what the Rust runtime lacks,
and what would unblock it, so the gap is a decision and not an omission. None
of them is in `current_rust_worker_toolkit_capability_snapshot.json`; a saved
toolkit of these types is still skipped with `agent_toolkit_skipped` on the
Rust worker, as before this batch.

## `delta_lake` (blocked)

SDK: `tools/aws/delta_lake/{__init__,api_wrapper,schemas,tool}.py`, registered
as `delta_lake` with `get_tools`. Configuration `delta_lake_configuration`
(`aws_access_key_id`, `aws_secret_access_key`, optional `aws_session_token`,
`aws_region`, `s3_path` or `table_path`). Three tools, all over the Python
`deltalake` (delta-rs) binding plus pandas and numpy:

| Tool | SDK behavior |
| --- | --- |
| `query_table` | `DeltaTable(path, storage_options).to_pandas()` loads the whole table, then `df[df[col] == val]` per `filters` pair, `df.query(query)` in the pandas expression language, `df[columns]`, and `json.dumps(df.to_dict(orient="records"), default=str)` |
| `vector_search` | Loads the whole table, drops null embeddings, cosine similarity in numpy, top `k` rows by similarity |
| `get_table_schema` | `dt.schema().to_pyarrow().to_string()` |

Why not in Rust: reading a Delta table is transaction-log replay (JSON commits
plus Parquet checkpoints) and Parquet/Arrow decoding from S3 with SigV4. The
agent runtime has none of these, and adding them means the `deltalake`,
`arrow`, `parquet` and `object_store` crate families. `query_table`'s `query`
argument is a pandas `DataFrame.query` expression, which has no Rust
interpreter; a different expression language would accept the same strings
with a different meaning. The Python worker image does not ship the
dependencies either (`delta_lake` is in the Python capability's
`unsupported_import_keys`), so no deployment serves this type today.

Unblock: an owner decision on a native table engine (the data-analysis row of
`configuration-toolsets.md` already points at Polars) and on the query
language, then a family over that engine with a bounded scan instead of a
whole-table load.
