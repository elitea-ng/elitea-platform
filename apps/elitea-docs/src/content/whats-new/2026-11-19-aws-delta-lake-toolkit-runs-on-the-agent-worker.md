---
date: 2026-11-19
order: 6
---
## AWS Delta Lake toolkit runs on the agent worker

The **AWS Delta Lake** toolkit now works: an agent can read a Delta Lake table in S3 with `query_table`, `vector_search` and `get_table_schema`. `query_table` keeps the SDK's `query` argument (pandas query syntax, literals only, read by a bounded parser) and adds structured `filters`, and every read is bounded in rows, bytes and time so a large table cannot exhaust the worker. The type stays out of the **New toolkit** picker, as in the SDK. Operators who restrict worker egress need do nothing: a run that uses the toolkit opens only the S3 endpoint of the credential's region. See [AWS Delta Lake](/integrations/toolkits/other-toolkits#aws-delta-lake).
