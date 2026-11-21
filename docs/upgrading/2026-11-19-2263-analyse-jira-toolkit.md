# Toolkits: the Jira analysis toolkit, `analyse_jira` (issue 2263, epic 2254)

**What changed.** A new toolkit type, `analyse_jira`, is served by the Rust worker. It reads Jira in bulk
(issues with their status history, sprints, statuses, field usage), writes the extracted data as CSV files in
an artifact bucket (default `analyse-jira`), and calculates flow metrics (lead time, cycle time, life time,
throughput, flow efficiency, flow load) over those files. It reuses the saved `jira` credential: no new
credential type, no database migration, no client contract bump. The upstream wrapper behind
`analyse_jira` is not public, so the tools are reconstructed from the surviving upstream test and the
behaviour of the `elitea-analyse` library; the toolkit's page lists what is kept, what is added and what is
deliberately not reproduced.

**Who is affected.** Operators who curate the toolkit types and the worker egress allowlist. The new type
appears in the create list and in Admin > Toolkits like any other.

**What to do.** Nothing is required. To keep the type off, hide it in Admin > Toolkits. Its Jira reads leave
the worker through the same egress allowlist as the `jira` toolkit (the Jira host from the saved credential),
so a deployment that already allows Jira needs no change. Its three dataset-writing tools (`get_jira_issues`,
`get_jira_sprints`, `calculate_jira_metrics`) need the project's artifact store and create the toolkit's
bucket when it does not exist; a run without one offers only the three read tools and says so. The tool
categories are `read` for `get_number_off_all_issues`, `get_jira_fields_overview` and `get_jira_statuses`,
and the default `write` for the rest, so an approval policy that names `write` asks before an extraction.
