---
title: "Retention policy and legal hold (#1495)"
issue: 1495
date: 2026-10-28
order: 41
---

* **Shared migration 0195** adds `elitea_runtime.retention_policy`,
  `elitea_runtime.legal_holds` and the journal
  `elitea_runtime.conversation_attachment_cleanup`. It seeds one platform
  row per data class with today's behaviour: conversations and attachments
  keep forever, `gateway_request_log` 92 days, `admin_audit` 365 days,
  `security_audit` keep forever. It grants `retention.manage` and
  `legal_hold.manage` to `super_admin` and `admin`. Nothing new is deleted
  after the upgrade.
* **elitea-main: new routine `main.conversation_retention`** (hourly). It
  deletes nothing until an administrator sets a maximum age for
  `conversation` on the admin Data retention page. Then it deletes
  conversations whose last message is older than the maximum, with their
  attachment bytes, except conversations under a legal hold.
* **Behaviour change: a user's delete of a conversation under an active
  legal hold answers 409.** No hold exists after the upgrade.
* **elitea-scheduler:** the audit and security audit outbox sweepers keep
  `AUDIT_RETENTION_DAYS` and `SECURITY_AUDIT_OUTBOX_RETENTION_DAYS` until an
  administrator saves the matching row. A saved row then wins. Both sweepers
  now always start; with the env value at 0 and no saved row they delete
  nothing, as before.
* **elitea-llm-gateway:** the request log prune reads the
  `gateway_request_log` row from the shared database on each hourly prune.
  It keeps 92 days when the row is missing or unreadable. No new setting.
  The gateway uses its existing `DATABASE_URL`; the role must be able to
  read `elitea_runtime.retention_policy` (the standard single-role install
  can).
