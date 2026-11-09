---
title: "Idle compaction per agent (#1610, ADR-0050 decision 9b2)"
issue: 1610
date: 2026-11-08
adr: ADR-0050
order: 97
---

Migration `shared/0230_agent_idle_compactions.sql` (additive: one table,
no backfill). Upgrade the workers (`elitea-worker-rust`) before Main. A
worker now opts in to idle compaction on its claims (`ClaimCommandRequestV1`
field 21, `idle_compaction`). Main answers an idle job claimed by an older
worker "retry later", so an idle check queued during a rolling upgrade waits
for an upgraded worker instead of running as an empty turn. Checks that wait
past their agent's cache lifetime are skipped.

- When an agent's turn settles, Main arms one idle check for that agent in
  that conversation. Four minutes after the turn's last model request
  started, while the prompt cache is still warm, it summarises the agent's
  session as a cached continuation of that request. The check is skipped
  when the model has no paid prompt cache, the conversation is under 60,000
  tokens, or the cache has already expired.
- The same agent's next turn cancels its check, or stops its running idle
  run. A peer agent's or a person's message does not.
- The idle run writes nothing to the transcript and sends no live event. Its
  spend is the agent's own execution spend.
- Cloud agent requests on native Anthropic models now mark the end of the
  conversation as a cache breakpoint, so later turns read it from the cache.
- `GET .../context_analytics/prompt_lib/{project}/{conversation}` gains
  `agent_contexts[]`, each agent participant's own meter. The chat's
  participant list shows it.
- An agent run from a desktop thread keeps one idle check per (thread,
  agent), not one per thread.
