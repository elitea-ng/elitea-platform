---
date: 2026-11-14
order: 3
---
## Pipeline layout, toolkit Test Settings and index history

Scrolling the settings panel on the left of the pipeline editor no longer scrolls the canvas away: the panel scrolls on its own. A pipeline whose editor is read-only (no permission to update pipelines in the project, or a published pipeline) now says so above the Flow and YAML views. Running `index_data` from a toolkit's **Test Settings** starts an index run, as the Indexes tab does, instead of failing with "does not support the index_data tool"; the result panel points to the Indexes tab. In an index's **History**, the first "Created" entry no longer reads "Successfully reindexed" with zero counts, and a failure's **Details for support** no longer shows the placeholder `welcome_message_id`.
