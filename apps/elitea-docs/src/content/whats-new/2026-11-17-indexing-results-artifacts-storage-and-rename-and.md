---
date: 2026-11-17
order: 2
---
## Indexing results, Artifacts storage and rename, and Applications

An index run in which every document failed to fetch and nothing is indexed is now reported as **Failed** with the reason in the message, not as **Partially indexed** with 0 documents. A run the embedding gateway stopped after the index held documents is reported as **Partially indexed** with the counts, not as failed. The result message names the ignored chunking settings only for the file types the run listed. An Artifacts index over a Google Cloud Storage bucket no longer skips every file as `fetch_failed`: the worker pins each download to the listed modified time when the store sends no entity tag. In Artifacts, the **Storage** menu always offers **Platform storage**, and renaming a file keeps its extension: the dialog shows it beside the name and Main refuses a rename that changes it. The **Applications** tab lists the Wikis and Inventory you created (they are toolkits of those types), and the App Catalog marks them **Configured**. Upgrade Main, the worker and the web app together.
