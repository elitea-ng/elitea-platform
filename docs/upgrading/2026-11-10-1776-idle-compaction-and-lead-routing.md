# Agents: idle compaction follow-ups and lead routing (#1776, #1804, ADR-0050)

- **What changed.**
  - **Idle compaction runs as the agent.** An idle compaction run (ADR-0050 decision 9b2) now carries the application
    version of the answer it summarizes, so that version's governed agent identity (ADR-0039) and release apply to it.
  - **Cancelled by any turn of the agent.** A turn of the same agent that names no participant (an older client, an
    ad hoc path) now cancels the agent's pending idle checks in the conversation.
  - **Analytics.** The run's model call is reported with `trigger_origin` `idle_compaction` in project analytics. It
    still counts in call, token and cost totals, but no longer makes the person whose turn it summarizes an active
    user. The `trigger_origin` descriptions in the API now list `idle_compaction`.
  - **Lead routing (#1804).** Under the `lead` floor, a message with no `addressed_to` goes to the lead even when
    `participant_id` names another agent. Only `addressed_to` addresses (ADR-0050 decision 7c). The web app's **To:**
    pick sends an address, so it is unaffected.
- **Who is affected.** Operators reading analytics by `trigger_origin`, and clients that relied on `participant_id`
  alone to pick an agent over the lead.
- **What to do.** Nothing for the platform: there are no migrations and no configuration changes. A client that
  picks an agent over the lead must send it in `addressed_to`.
