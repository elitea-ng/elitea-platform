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

## `aws` (blocked)

SDK: `tools/cloud/aws/{__init__,api_wrapper}.py`. Inline settings `region`,
`access_key_id`, secret `secret_access_key`; one tool, `execute_aws`, whose
`query` (JSON text or object) names `service`, `method_name` and
`method_arguments` and is meant to call that boto3 client method and return
`str(response)`.

The pinned SDK cannot materialize it, in three independent ways:

- `tools/__init__.py` registers `aws` with no `get_tools`
  (`_safe_import_tool('aws', 'cloud.aws', None, 'AWSToolkit')`), so the SDK
  runtime binds no tool for a saved `aws` toolkit;
- `AWSToolConfig.validate_toolkit` calls `boto3.client('service', ...)`, and
  `service` is not an AWS service name, so construction raises
  `UnknownServiceError`;
- `execute_aws` calls the client object (`self._client(service=...)`), which is
  not callable.

The Python worker image does not ship boto3 either (`aws` is in the Python
capability's `unsupported_import_keys`).

Why not in Rust: the contract is boto3's dynamic operation dispatch. Turning
`{"service": "s3", "method_name": "list_objects_v2", ...}` into a request needs
botocore's service models: the per-service protocol (`query`, `ec2`, `json`,
`rest-json`, `rest-xml`), endpoint and signing names, operation names and
input/output shapes. The Rust ecosystem has per-service SDK crates, not a
model-driven dynamic client, and the agent runtime has no SigV4 signer (the
`gcp` family's signing is an RS256 JWT grant, which does not apply). A
GCP-style generic signed HTTP tool would not honor this argument contract, so
it would be a different tool under the same name.

Unblock: an owner decision between (a) embedding a bounded subset of the
botocore service models with a SigV4 signer and a model-driven serializer, or
(b) a new tool contract (generic SigV4 request) under a new name, with the SDK
schema left unserved.

## `memory` (blocked pending an ownership decision)

SDK: `tools/memory/__init__.py`, dispatched by `runtime/toolkits/tools.py`
(`tool['type'] == 'memory'`). Settings `namespace` (default: the toolkit id),
`pgvector_configuration.connection_string` and `selected_tools` (default
`manage_memory`, `search_memory`). The worker passes no store, so
`MemoryToolkit.get_toolkit` opens a psycopg connection to the configured
database through `store_manager.StoreManager` and runs LangGraph
`PostgresStore.setup()` migrations there. Four tools over the namespace
`(namespace,)`:

| Tool | SDK behavior |
| --- | --- |
| `manage_memory` | `store.put` of `{"content": ..., "metadata"?: ...}` under `key` or a new UUID4; returns `Successfully stored memory with key: <key>` |
| `search_memory` | `store.search(..., query=query, limit=limit)`; the store is built without an index, so the query text does not rank anything and the most recently updated items come back; returns `json.dumps([...], indent=2)` of `key`, `content`, optional `metadata`, or `No memories found matching your query.` |
| `get_memory` | `store.get`; `json.dumps(..., indent=2)` or `No memory found with key: <key>` |
| `delete_memory` | `store.delete`; `Successfully deleted memory with key: <key>` |

Why not in Rust: the four operations themselves are portable (the runtime
already links sqlx behind `toolkit-sql`), but `long-term-memory.md` (gate 7b)
assigns long-term memory persistence to Main's existing memories repository
and states that no memory dependency or runtime behavior is activated until
that gate closes. Porting the SDK as written would make the worker hold a
direct connection to a user-configured database of cross-conversation data
and would create the second memory store that the gate forbids. That is an
owner decision, not a porting detail.

Unblock: choose (a) SDK compatibility, with the four tools over the LangGraph
`store` table through sqlx so memories written by the Python worker stay
readable, or (b) the gate 7b direction, with the four tool names and schemas
backed by Main's memories API through a claim-scoped host interface (the
`artifact` family's lent authority is the pattern), plus a migration of
existing LangGraph-store memories.
