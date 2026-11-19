# Indexing: the v1 task-restamp check and its columns are retired (issue 1382)

**What changed.** The index v2 cutover preflight (`/index-v2-preflight`) no longer counts
`pending_task_restamps`, and its JSON report no longer carries that field. Shared migration 0256 drops the
ten `index_meta_task_restamp_*` / `index_meta_task_restamped_at` columns (with their CHECK constraints and
the pending partial index) from `elitea_runtime.index_ingest_jobs`. Migration 0049 added them for an effect
whose last writer was removed with the pgvector store (#1378); nothing reads them any more. The toolkit
schema snapshot's text "the pgvector connection pool" now reads "the vector store connection pool".

**Who is affected.** Operators who script against the preflight report, and anyone who queries the dropped
columns directly. No supported deployment still holds v1 index rows.

**What to do.** Nothing for the migration: Main's migrate job applies 0256, and the drop is not reversible
from the application side, so take the usual database backup first. If a script reads
`persisted.pending_task_restamps` from the preflight report, remove that read.
