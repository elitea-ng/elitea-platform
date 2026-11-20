---
date: 2026-10-30
order: 6
---
## Approval ledger and unattended runs

Every pause and every decision of a sensitive tool or a pipeline review node is now a row in an append-only approval ledger, with the actor, the tool, the toolkit, the digest of the arguments and the decision. Each row is also a hash-chained `agent.approval` event in the security audit log, for every run. When two people decide the same step at the same time, one decision applies and the other gets HTTP 409. **Runs that a schedule or a trigger started never approve a step.** By default (`fail`), such a run now ends with `approval_required_unattended` when it gets to a step that needs approval. Set `on_approval_required` on a schedule or a trigger to `skip` (the model is told that the step was skipped) or `notify_and_wait` (the owner is notified and the run waits, 24 hours by default). The platform default is under **Configuration › Approvals**. See [Approval evidence and unattended runs](/admin/enterprise-controls/runtime-safety#approval-evidence-and-unattended-runs).
