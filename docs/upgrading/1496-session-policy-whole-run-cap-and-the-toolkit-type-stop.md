---
title: "Session policy, whole-run cap and the toolkit-type stop (#1496, #1498)"
issue: 1496
date: 2026-10-29
order: 44
---

* **Shared migration 0200** adds to `elitea_auth.browser_sessions` a
  non-secret reference `ref` (an identity column, backfilled for existing
  rows), `ended_at`, `end_reason` and two audit markers. Adding the identity
  column rewrites the table once; it is small in every deployment we know.
* **Shared migration 0201** adds `execution_jobs.browser_session_ref` and
  `execution_jobs.toolkit_types`. Existing rows read NULL.
* **New audit events, `event_type=session`.** A single-replica job writes
  `session.start`, `session.expire`, `session.logout` and `session.revoke`
  into `centry.audit_events` about once a minute. The migration marks every
  existing session's start, and every session that had already ended or
  lapsed, as reported, so the first pass writes no backlog: sessions open at
  the upgrade get their end event only. If you export audit events, expect
  about two events per browser sign-in from then on.
* **Configuration › Session Policy** (maximum idle timeout and maximum
  lifetime) and **Configuration › Run Caps** (maximum run time) are new
  console sections. All values default to 0, which keeps today's behaviour.
  Nothing to do unless you want the bounds.
* **A `toolkit_type` suspension now cancels runs in flight** that hold a
  toolkit of that type, as the agent, project and platform scopes already
  did. Its `cancelled_runs` is no longer always 0.
