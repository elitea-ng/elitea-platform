---
date: 2026-10-28
order: 7
---
## Run step records and chained audit trail

Every agent run now keeps a step record of each model call, tool call, retrieval (with the documents it returned and their scores), approval, delegation, cap, error and its answer, in order and tied to the agent release. The records are hash-chained per run, anchored in the security audit log, exported to your SIEM as the `run_steps` stream, and readable over the API with the new `runs.evidence.read` permission. Step content is kept by reference under the new `step_content` retention class, and a toolkit's `evidence_fields` setting limits what is kept of its results. The admin audit trail is now hash-chained too, with a verification endpoint. Both new retention classes keep forever by default, and no existing data is deleted. See [Audit evidence and retention](/admin/enterprise-controls/audit-evidence-and-retention) and [Reconstruct an agent run](/admin/enterprise-controls/reconstruct-a-run).
