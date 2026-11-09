---
title: "Agent personas: the version's soul (#1604, ADR-0050 decision 1a)"
issue: 1604
date: 2026-11-06
adr: ADR-0050
order: 79
---

Upgrade the workers (`elitea-worker-rust`) before Main. Tenant migration
0160 adds the nullable `application_versions.soul` (jsonb) column. It is
empty for every existing version, so there is nothing to backfill.

A version can now carry a soul (`AgentSoulV1`): the agent's display name,
voice, values, boundaries and relationships, and a free-form body. The
agent editor's new Persona tab edits it. Main sends it in the version
details, and the worker puts it into the prompt after the platform text and
the guardrails, before the instructions. A worker from an earlier release
ignores the soul, so an agent with a soul would answer without it.

Release digests do not change for a version without a soul. Adding,
editing or removing a soul changes the version's digest, as any other
configuration edit does. A write that names a capability (tools,
credentials, permissions and the like) is refused with 400
`soul_capability_key`, and a soul over 16384 bytes of text with 400
`soul_too_large`.

The desktop app reads a `SOUL.md` next to a Local work folder's
`AGENTS.md`.
