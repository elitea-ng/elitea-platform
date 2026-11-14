# Indexing: run summary in the index view, readable failure reasons, refused start without an embedding model

Issues #2041 and #2042.

**What changed.** The `index.ingest.completed` event of an index run now carries
the worker's typed counts next to `status` and `message` (`indexed_documents`,
`indexed_chunks`, `failed_chunks`, `unchanged_documents`, `total_documents`,
`skipped`); each is absent unless the worker
stated it, so a run from the Python worker keeps the old two-field event. The
web index view shows a running state at once and the summary when the event
arrives. A model-gateway refusal in an index run's failure message is now a
sentence (the fault code stays in brackets), and the index shows that reason.
`POST /elitea_core/test_toolkit_tool/...` for `index_data` answers `422` with
`{"error": "No embedding model is available to this project. ..."}` when the
toolkit's embedding model is blank or absent and the project has none to default to,
instead of admitting a run that fails.

**Who is affected.** Operators of deployments that run indexes. There is no
migration and no setting.

**What to do.** Upgrade Main, the worker and the web app together. An older web
app ignores the new event fields; an older worker sends the previous failure
text, which the new web app turns into a sentence itself.
