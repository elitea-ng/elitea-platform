---
date: 2026-11-14
order: 7
---
## Index runs show their progress and result, and a failed index says why

When you start an index, the panel now shows a running state at once instead of "No messages yet", and when the run finishes it shows a summary in the same place, without a reload: the outcome, documents and chunks indexed, documents already indexed, what was skipped and why, chunks that failed, how many documents the index holds now, and how long it took. A failed index shows its reason next to its name and in the list tooltip, in plain words: a model the project may not use (`model_gateway.forbidden`) reads "this project is not allowed to use that model…" with what to do about it, and the code stays in brackets for support. A start in a project with no embedding model is refused with that reason instead of creating a run that fails. While the run is going, the panel lists its steps as the worker reports them: reading the source and the index (with what it found to index, keep and remove), then one "Indexing documents" step whose count of documents and chunks updates every few documents (at most twenty updates for a very large index). Progress is best effort: a broken output stream does not fail the index, it only stops the step list, and the final result is still recorded. **Upgrade Main, the worker and the web app together.**
