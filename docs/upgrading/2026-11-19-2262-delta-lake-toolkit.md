# Agent worker: the AWS Delta Lake toolkit is served (issue 2262, epic 2254)

**What changed.** The `delta_lake` toolkit type, which the worker withheld until now, is built by the agent
worker. It reads one Delta Lake table in S3 with `query_table`, `vector_search` and `get_table_schema`. The
worker image links delta-kernel-rs (Arrow and Parquet) behind a worker-only cargo feature,
`toolkit-delta-lake`, so the worker binary grows; the desktop host does not link it. Main's worker capability
snapshot now lists `delta_lake`, so the type no longer answers "this deployment's agent worker does not support
the delta_lake toolkit".

The type stays `hidden` in the **New toolkit** picker, as in the SDK. An existing toolkit, one created through the
API, or one imported from the SDK now runs. The pandas `query` argument of `query_table` is read by a bounded parser for its literal-only subset
(comparisons, and/or/not, in, is null, arithmetic; no function calls), and every read is bounded in rows, bytes
and time.

**Who is affected.** Operators who restrict worker egress with an allowlist, and anyone sizing the worker image.
No database migration, no contract bump, no new configuration.

**What to do.** Upgrade the worker before Main, so Main never offers a type the worker cannot build. Egress needs
no change: a run that uses a `delta_lake` toolkit opens the one S3 endpoint of the credential's region
(`s3.<region>.amazonaws.com`, `.amazonaws.com.cn` for a `cn-` region), derived from the credential and never from
a user-supplied host. A platform-wide allowlist that blocks S3 must allow that host for these toolkits to run.
