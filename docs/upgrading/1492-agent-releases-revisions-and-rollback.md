---
title: "Agent releases, revisions and rollback (#1492)"
issue: 1492
date: 2026-10-29
order: 45
---

* **Shared migration 0198** creates `elitea_lifecycle.application_releases`
  (immutable: an UPDATE trigger refuses every change except withdrawal),
  `application_release_heads` (the current release per agent) and
  `version_revisions`. **Shared migration 0199** adds
  `agent_execution_jobs.release_id`. No data is copied: versions published
  before the upgrade have no release until they are published again, and
  run as before.
* **Published versions are read-only on every route.** The HTTP check that
  refused saving a published version is now enforced in the repository, so
  an application PUT that carried a nested published version (which skipped
  the check) is refused too. Edit a draft and publish it instead.
* **Every version save writes a revision** in the same transaction. A
  database built without migration 0198 (for example a hand-made dev
  database) fails every save until it is migrated.
* **New 409 answers on run start:** `release_toolkit_drifted` when a run
  names a release (or the agent was rolled back to one) whose toolkit
  settings or configurations changed since it was published, and
  `release_withdrawn_dependencies_removed` when a run names a withdrawn
  release of an agent with sub-agents (unpublish deletes the sub-agent
  copies its publish made). The rollback refusals are
  `application_not_published`, `release_already_published`,
  `release_withdrawn`, and 400 for a reason longer than 200 characters. A
  run of the live published version is never refused for drift.
* **Publish and unpublish are all-or-nothing.** A publish now embeds the
  sub-agent copies and records the release in its own transaction: a
  failed copy or release answers 500 and publishes nothing (a failed
  sub-agent copy used to answer 200 with the loss only in the log). An
  unpublish reverts the version, removes the Catalog copy and withdraws its
  releases in one transaction: a failure leaves the version published and
  the retry works.
* **Rollback and mandate-bound triggers.** With credential governance on,
  an inbound trigger of an agent that was rolled back answers 403
  `MANDATE_RELEASE_ROLLED_BACK`: its mandate binds the trigger's version,
  not the release's. Roll the agent forward, or point the trigger at the
  release's version with a mandate bound to it.
