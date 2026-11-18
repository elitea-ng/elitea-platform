# Toolkits: List indexes names every index on the Indexes tab (issue 2150), and the index result bucket is hidden (issue 2167)

**What changed.** The index catalog Main freezes into an indexing toolkit (Artifact, GitHub, GitLab,
Bitbucket, Azure DevOps, Confluence, Jira, SharePoint, Figma, TestRail, qTest, Xray, Zephyr) now holds
every live registry row, the rows the **Indexes** tab lists. A row that has not stored vectors (never run,
or a first run that failed) is an entry with a `name`, an `index_id` and a `state` and no
`embedding_model` or `embedding_dimension`. The worker lists it in `list_indexes` and answers a search or
removal of it by name with "holds no searchable content yet" instead of "not found". Before, such rows were
left out and the tool said "No indexed collections".

Separately, the platform's own `index-artifacts` bucket (index results and `toolkit-discovery` files) is a
hidden bucket: it is not listed under Artifacts and every bucket route answers it as absent. No migration;
nothing is moved or deleted, so existing projects need no clean-up.

**Who is affected.** Operators running the Rust indexing runtime (`ELITEA_INDEXING_RUNTIME=rust`). A worker
from before this change refuses a catalog entry that has no embedding space, and the toolkit's index tools
fail for that run.

**What to do.** Upgrade the worker before Main (or together). Nothing to configure. The API is unchanged
and the client contract is not bumped.
